package main

import (
	"context"
	"fmt"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// restoreCompletedReceivePaths runs only through live, locked terminal
// maintenance admission. It reuses additive preparation for the selected
// receivers. It never recreates a legacy peer, selects egress, or retires devices.
func restoreCompletedReceivePaths(ctx context.Context, devices receiverDevices, cfg config, key wgtypes.Key, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !view.Complete || (view.Round.Phase != handover.Retire && view.Round.Phase != handover.Abort) || !reflect.DeepEqual(intent.Views[plan.MachineUID], view) || !reflect.DeepEqual(intent.Plans[plan.MachineUID], plan) {
		return fmt.Errorf("transport restoration requires exact completed journal intent")
	}
	scope := receiveJournalScope{Namespace: plan.Namespace, MeshName: plan.MeshName, MeshUID: plan.MeshUID, MachineUID: plan.MachineUID}
	uid, err := receiveJournalParticipant(plan, scope, cfg.nodeName, key)
	if err != nil || uid != nodeUID {
		return fmt.Errorf("transport restoration participant differs: %v", err)
	}
	legacy, selectedLegacy := intent.Legacy[plan.MachineUID]
	if selectedLegacy != (view.RollbackRequested || view.Round.Phase == handover.Abort) || (selectedLegacy && !reflect.DeepEqual(legacy, plan)) {
		return fmt.Errorf("transport restoration selection differs from completed journal")
	}
	expected, err := tunnel.ReceiveTransitionSpec(plan, view.Round.ClusterUID, view.Round.PlanHash)
	if err != nil {
		return err
	}
	expected.Phase = view.Round.Phase
	if view.RollbackRequested && view.Round.Phase != handover.Abort {
		expected.FromGeneration, expected.ToGeneration = expected.ToGeneration, expected.FromGeneration
		for i := range expected.Members {
			member := &expected.Members[i]
			member.FromKey, member.ToKey = member.ToKey, member.FromKey
			member.FromPeerKey, member.ToPeerKey = member.ToPeerKey, member.FromPeerKey
		}
	}
	serving := expected.ToGeneration
	if view.Round.Phase == handover.Abort {
		serving = expected.FromGeneration
	}
	if !reflect.DeepEqual(expected, view.Round.Spec) || view.ServingGeneration != serving {
		return fmt.Errorf("transport restoration generation differs")
	}
	if selectedLegacy {
		return nil // Receivers were retired; never revive them.
	}
	if err := prepareReceivePhasePaths(devices, cfg, key, plan, nodeUID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return verifyReceivePhasePaths(devices, cfg, key, plan, nodeUID)
}

// restoreCompletedReceiveState is called only under the executor's live
// completed-journal admission and host lock. Generation admission precedes
// allocation recovery; phase verification still requires native transport.
func restoreCompletedReceiveState(ctx context.Context, devices receiverDevices, cfg config, key wgtypes.Key, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes) error {
	if err := restoreCompletedReceivePaths(ctx, devices, cfg, key, plan, view, nodeUID, intent); err != nil {
		return err
	}
	own, allocated, err := tunnel.ReceiveLocalNetwork(plan, nodeUID)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := restoreReceiveRoot(ctx, devices, cfg, key, plan.MeshUID, nodeUID); err != nil {
		return err
	}
	if err := restoreReceiveLegacyPeers(ctx, devices, cfg, key, nodeUID, intent); err != nil {
		return err
	}
	_, err = reconcileStableAddressOwner(devices, stableAddressOwnerRequest{MeshUID: plan.MeshUID, NodeUID: nodeUID, LegacyInterface: cfg.iface, RootPublicKey: key.PublicKey().String(), Addresses: allocated}, true)
	if err != nil {
		return err
	}
	return restoreCompletedReceiveRoutes(ctx, devices, cfg, key, own, allocated, plan, view, nodeUID, intent)
}
