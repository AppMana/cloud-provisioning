package main

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type admittedReceiveRetirement struct {
	attachment.ReceiveRetirementSet
	NodeUID, BootID string
}

// Historical finalization suppresses only receipt renewal. Native cleanup and
// the durable local fence remain mandatory, including on a different boot.
// Use the frozen journal's observation policy; a policy mismatch fails closed.
func receiveRetirementFinalized(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, admitted admittedReceiveRetirement) (bool, error) {
	if len(admitted.Inventory.Publications[scope.MachineUID].RetirementFinalization) == 0 {
		return false, nil
	}
	view, err := admitted.Journals[scope.MachineUID].Transition().View()
	if err != nil {
		return false, err
	}
	source, verified, err := attachment.ReadReceiveRetirementFinalization(ctx, api, store, scope.Namespace, scope.MeshName, scope.MeshUID, scope.MachineUID, string(admitted.Metadata.UID), view.Round.MaxReceiptAge)
	if err != nil {
		return false, err
	}
	current := admittedReceiveRetirement{ReceiveRetirementSet: source, NodeUID: admitted.NodeUID, BootID: admitted.BootID}
	if !verified || !sameReceiveRetirement(admitted, current) {
		return false, fmt.Errorf("retirement finalization changed during admission")
	}
	return true, ctx.Err()
}

// Recheck an already admitted cleanup attempt without repeatedly reconstructing
// its immutable ancestry at every device boundary. The shared recheck still
// reads every live journal, inventory and mesh; only unchanged authority or
// appended observations are accepted. Execution must perform full admission
// again before issuing its receipt. Never use this as initial admission.
func recheckReceiveRetirement(ctx context.Context, api kubernetes.Interface, store handover.Store, admitted admittedReceiveRetirement, nodeName string, key wgtypes.Key) error {
	if api == nil || nodeName == "" || key == (wgtypes.Key{}) || admitted.NodeUID == "" || admitted.BootID == "" {
		return fmt.Errorf("retirement recheck requires admitted local identity")
	}
	found := false
	for _, consumer := range admitted.Withdrawal.Consumers {
		if consumer.NodeName == nodeName && consumer.NodeUID == admitted.NodeUID && (consumer.PublicKey == key.PublicKey().String() || consumer.Site && consumer.PublicKey == "") {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("retirement consumer identity changed")
	}
	boot, err := localReceiveBootID()
	if err != nil {
		return err
	}
	if boot != admitted.BootID {
		return fmt.Errorf("retirement local boot changed")
	}
	node, err := api.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := validateReceiveParticipantNode(node, admitted.NodeUID, admitted.BootID); err != nil {
		return err
	}
	if err := admitted.Recheck(ctx, api, store); err != nil {
		return err
	}
	current, err := api.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !sameReceiveParticipantNode(node, current) {
		return fmt.Errorf("retirement participant changed during recheck")
	}
	return ctx.Err()
}

// loadReceiveRetirement admits the full reserved set and the persisted survivor
// identity without requiring the deleted worker's adoption Secret or Node. It
// never drops a journal because its worker is absent. Its output is read-only
// intent: native cleanup must hold the host lock, checkpoint retirement, verify
// exact ownership, and re-admit before publishing an authenticated receipt.
func loadReceiveRetirement(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, nodeName string, key wgtypes.Key) (admittedReceiveRetirement, error) {
	fail := func(err error) (admittedReceiveRetirement, error) { return admittedReceiveRetirement{}, err }
	if api == nil || nodeName == "" || key == (wgtypes.Key{}) || scope.MachineUID == "" || store.Name != tunnel.ReceiveTransitionName(scope.MachineUID) {
		return fail(fmt.Errorf("receiver retirement requires admitted scope and local identity"))
	}
	shared, err := attachment.ReadReceiveRetirementSet(ctx, api, store, scope.Namespace, scope.MeshName, scope.MeshUID, scope.MachineUID)
	if err != nil {
		return fail(err)
	}
	result := admittedReceiveRetirement{ReceiveRetirementSet: shared}
	for _, c := range result.Withdrawal.Consumers {
		if c.NodeName == nodeName {
			if c.PublicKey != key.PublicKey().String() && !(c.Site && c.PublicKey == "") {
				return fail(fmt.Errorf("retirement consumer key changed"))
			}
			result.NodeUID = c.NodeUID
		}
	}
	if result.NodeUID == "" {
		return fail(fmt.Errorf("local node is not a retained retirement consumer"))
	}
	result.BootID, err = localReceiveBootID()
	if err != nil {
		return fail(err)
	}
	node, err := api.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if err := validateReceiveParticipantNode(node, result.NodeUID, result.BootID); err != nil {
		return fail(err)
	}
	if err := shared.Recheck(ctx, api, store); err != nil {
		return fail(err)
	}
	currentNode, err := api.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if !sameReceiveParticipantNode(node, currentNode) {
		return fail(fmt.Errorf("receiver retirement source changed during admission"))
	}
	if err := validateReceiveParticipantNode(currentNode, result.NodeUID, result.BootID); err != nil {
		return fail(err)
	}
	return result, nil
}
