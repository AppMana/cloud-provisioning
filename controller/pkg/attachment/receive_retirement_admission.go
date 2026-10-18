package attachment

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ReceiveRetirementSet is live source admission shared by controllers and agents.
// It retains every reserved journal, including workers whose API objects are gone.
// Consumer authentication and native absence remain separate requirements.
type ReceiveRetirementSet struct {
	Inventory              tunnel.ReceiveInventory
	Metadata, MeshMetadata metav1.ObjectMeta
	Mesh                   *corev1.Secret
	Journals               map[string]handover.Snapshot
	Plans                  []tunnel.ReceiveTransitionPlan
	Withdrawal             RemoteWithdrawalIntent
}

// ReadReceiveRetirementSet validates all reserved histories, route ownership,
// persisted withdrawal and publication, then rereads their revisions. Callers
// must recheck again around authentication or mutation; sequential API reads
// are not an atomic cluster snapshot.
func ReadReceiveRetirementSet(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID string) (ReceiveRetirementSet, error) {
	return readReceiveRetirementSetStage(ctx, api, store, namespace, meshName, meshUID, machineUID, true)
}

// ReadPreparedReceiveRetirementSet performs the same full-inventory validation
// before publication. It grants no agent execution or native cleanup authority.
func ReadPreparedReceiveRetirementSet(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID string) (ReceiveRetirementSet, error) {
	return readReceiveRetirementSetStage(ctx, api, store, namespace, meshName, meshUID, machineUID, false)
}

func readReceiveRetirementSetStage(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID string, withdrawn bool) (ReceiveRetirementSet, error) {
	fail := func(err error) (ReceiveRetirementSet, error) { return ReceiveRetirementSet{}, err }
	if api == nil || store.ConfigMaps == nil || machineUID == "" || store.Name != tunnel.ReceiveTransitionName(machineUID) {
		return fail(fmt.Errorf("retirement requires live reserved scope"))
	}
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, namespace, meshName, meshUID)
	if err != nil {
		return fail(err)
	}
	mesh, err := api.CoreV1().Secrets(namespace).Get(ctx, meshName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if string(mesh.UID) != meshUID || mesh.DeletionTimestamp != nil {
		return fail(fmt.Errorf("retirement mesh incarnation changed"))
	}
	result := ReceiveRetirementSet{Inventory: inventory, Metadata: *index.ObjectMeta.DeepCopy(), MeshMetadata: *mesh.ObjectMeta.DeepCopy(), Mesh: mesh.DeepCopy(), Journals: map[string]handover.Snapshot{}}
	for _, uid := range inventory.MachineUIDs {
		currentStore := store
		currentStore.Name = tunnel.ReceiveTransitionName(uid)
		// ExpectedUID, when supplied, belongs only to the target journal.
		if uid != machineUID {
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
		if plan.MachineUID != uid || plan.Namespace != namespace || plan.MeshName != meshName || plan.MeshUID != meshUID {
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
	snapshot, found := result.Journals[machineUID]
	if !found {
		return fail(fmt.Errorf("retiring journal is not reserved"))
	}
	result.Withdrawal, err = readReceiveWithdrawalStage(inventory, machineUID, snapshot, mesh, withdrawn)
	if err != nil {
		return fail(err)
	}

	if err := result.Recheck(ctx, api, store); err != nil {
		return fail(err)
	}
	return result, nil
}

// Recheck detects changes to the full reserved set, mesh or inventory. It does
// not retry against newer state and does not treat missing workers as omissions.
func (s ReceiveRetirementSet) Recheck(ctx context.Context, api kubernetes.Interface, store handover.Store) error {
	if api == nil || store.ConfigMaps == nil || s.Mesh == nil || len(s.Journals) == 0 {
		return fmt.Errorf("retirement recheck requires admitted source state")
	}
	for uid, before := range s.Journals {
		currentStore := store
		currentStore.Name = tunnel.ReceiveTransitionName(uid)
		currentStore.ExpectedUID = string(before.Metadata().UID)
		current, err := currentStore.Load(ctx)
		if err != nil {
			return err
		}
		if err := current.ContinuesExecution(before); err != nil {
			return fmt.Errorf("reserved journal changed during retirement admission: %w", err)
		}
	}
	_, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, s.Mesh.Namespace, s.Mesh.Name, string(s.Mesh.UID))
	if err != nil {
		return err
	}
	mesh, err := api.CoreV1().Secrets(s.Mesh.Namespace).Get(ctx, s.Mesh.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	same := func(a, b metav1.ObjectMeta) bool {
		return a.UID != "" && a.UID == b.UID && a.ResourceVersion != "" && a.ResourceVersion == b.ResourceVersion && a.DeletionTimestamp == nil && b.DeletionTimestamp == nil
	}
	if !same(s.Metadata, index.ObjectMeta) || !same(s.MeshMetadata, mesh.ObjectMeta) {
		return fmt.Errorf("retirement source changed during admission")
	}
	return nil
}
