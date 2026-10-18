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
