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
	Inventory       tunnel.ReceiveInventory
	Metadata        metav1.ObjectMeta
	MeshMetadata    metav1.ObjectMeta
	Journals        map[string]handover.Snapshot
	Plans           []tunnel.ReceiveTransitionPlan
	Withdrawal      attachment.RemoteWithdrawalIntent
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
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	if err != nil {
		return fail(err)
	}
	mesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if string(mesh.UID) != scope.MeshUID || mesh.DeletionTimestamp != nil {
		return fail(fmt.Errorf("retirement mesh incarnation changed"))
	}
	result := admittedReceiveRetirement{Inventory: inventory, Metadata: *index.ObjectMeta.DeepCopy(), MeshMetadata: *mesh.ObjectMeta.DeepCopy(), Journals: map[string]handover.Snapshot{}}
	for _, uid := range inventory.MachineUIDs {
		currentStore := store
		currentStore.Name = tunnel.ReceiveTransitionName(uid)
		// ExpectedUID, when supplied, belongs only to the target journal.
		if uid != scope.MachineUID {
			currentStore.ExpectedUID = ""
		}
		snapshot, err := currentStore.Load(ctx)
		if err != nil {
			return fail(err)
		}
		if err := inventory.CheckJournal(uid, string(snapshot.Metadata().UID)); err != nil {
			return fail(err)
		}
		if _, found := snapshot.Predecessor(); found {
			if _, err := currentStore.ArchiveNames(ctx, snapshot); err != nil {
				return fail(err)
			}
		}
		if err := tunnel.ValidateReceiveTransportHistory(ctx, currentStore, snapshot); err != nil {
			return fail(err)
		}
		plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
		if err != nil {
			return fail(err)
		}
		if plan.MachineUID != uid || plan.Namespace != scope.Namespace || plan.MeshName != scope.MeshName || plan.MeshUID != scope.MeshUID {
			return fail(fmt.Errorf("retirement journal differs from reserved scope"))
		}
		result.Journals[uid] = snapshot
		result.Plans = append(result.Plans, plan)
	}
	if _, err := tunnel.PlanReceiveRouteSet(result.Plans); err != nil {
		return fail(err)
	}
	peers, err := tunnel.SitePeers(mesh.Data)
	if err != nil {
		return fail(err)
	}
	if peers == nil {
		peers = []tunnel.PeerSpec{}
	}
	// Withdrawal does not release the target's ranges. Another published owner
	// cannot acquire them while its retained native references are being retired.
	if _, err := tunnel.PlanReceiveNeighborPeers(peers, result.Plans); err != nil {
		return fail(err)
	}
	snapshot, found := result.Journals[scope.MachineUID]
	if !found {
		return fail(fmt.Errorf("retiring journal is not reserved"))
	}
	result.Withdrawal, err = attachment.ReadReceiveWithdrawal(inventory, scope.MachineUID, snapshot, mesh)
	if err != nil {
		return fail(err)
	}
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
	for _, uid := range inventory.MachineUIDs {
		currentStore := store
		currentStore.Name = tunnel.ReceiveTransitionName(uid)
		if uid != scope.MachineUID {
			currentStore.ExpectedUID = ""
		}
		current, err := currentStore.Load(ctx)
		if err != nil {
			return fail(err)
		}
		if err := current.ContinuesExecution(result.Journals[uid]); err != nil {
			return fail(fmt.Errorf("reserved journal changed during retirement admission: %w", err))
		}
	}
	_, currentIndex, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	if err != nil {
		return fail(err)
	}
	currentMesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	currentNode, err := api.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if !sameReceiveRevision(index.ObjectMeta, currentIndex.ObjectMeta) || !sameReceiveRevision(mesh.ObjectMeta, currentMesh.ObjectMeta) || !sameReceiveRevision(node.ObjectMeta, currentNode.ObjectMeta) {
		return fail(fmt.Errorf("receiver retirement source changed during admission"))
	}
	if err := validateReceiveParticipantNode(currentNode, result.NodeUID, result.BootID); err != nil {
		return fail(err)
	}
	return result, nil
}
