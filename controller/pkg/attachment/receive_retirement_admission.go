package attachment

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	Routes                 tunnel.ReceiveSurvivorRoutes
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

	// Keep all ownership, but select serving routes only for workers whose
	// withdrawal has not been published. Validate every retirement record;
	// a different worker's malformed record must not be silently filtered.
	withdrawnJournals := map[string]string{}
	var snapshots []handover.Snapshot
	for _, uid := range inventory.MachineUIDs {
		snapshot := result.Journals[uid]
		snapshots = append(snapshots, snapshot)
		if len(inventory.Publications[uid].Retirement) == 0 {
			continue
		}
		if _, err := ReadReceiveWithdrawal(inventory, uid, snapshot, mesh); err == nil {
			withdrawnJournals[uid] = string(snapshot.Metadata().UID)
			continue
		}
		// A captured but unpublished withdrawal still owns serving intent.
		if _, err := readReceiveWithdrawalStage(inventory, uid, snapshot, mesh, false); err != nil {
			return fail(err)
		}
	}
	result.Routes, err = tunnel.PlanReceiveSurvivorRoutes(snapshots, store.ClusterUID, withdrawnJournals)
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
	snapshots := make([]handover.Snapshot, 0, len(s.Journals))
	for uid, before := range s.Journals {
		if before.Metadata().Name != tunnel.ReceiveTransitionName(uid) {
			return fmt.Errorf("reserved journal differs from admitted machine")
		}
		snapshots = append(snapshots, before)
	}
	if err := store.RecheckExecutions(ctx, snapshots); err != nil {
		return fmt.Errorf("reserved journal changed during retirement admission: %w", err)
	}
	_, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, s.Mesh.Namespace, s.Mesh.Name, string(s.Mesh.UID))
	if err != nil {
		return err
	}
	mesh, err := api.CoreV1().Secrets(s.Mesh.Namespace).Get(ctx, s.Mesh.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	indexErr := retirementSourceRevision(s.Metadata, index.ObjectMeta, "configmaps")
	meshErr := retirementSourceRevision(s.MeshMetadata, mesh.ObjectMeta, "secrets")
	// A concurrent revision on one source must not hide replacement or
	// deletion of the other source behind a retryable conflict.
	if indexErr != nil && !apierrors.IsConflict(indexErr) {
		return indexErr
	}
	if meshErr != nil && !apierrors.IsConflict(meshErr) {
		return meshErr
	}
	if indexErr != nil {
		return indexErr
	}
	return meshErr
}

// A revision conflict invalidates the captured admission. Callers may discard
// it and admit a fresh source, but must not retry native work using that stale
// evidence. Replacement, deletion and incomplete metadata are not conflicts.
func retirementSourceRevision(before, current metav1.ObjectMeta, resource string) error {
	if before.UID == "" || before.UID != current.UID || before.Name == "" || before.Name != current.Name ||
		before.Namespace == "" || before.Namespace != current.Namespace ||
		before.ResourceVersion == "" || current.ResourceVersion == "" ||
		before.DeletionTimestamp != nil || current.DeletionTimestamp != nil {
		return fmt.Errorf("retirement %s identity or lifecycle changed during admission", resource)
	}
	if before.ResourceVersion != current.ResourceVersion {
		return apierrors.NewConflict(schema.GroupResource{Resource: resource}, current.Name,
			fmt.Errorf("retirement source revision changed during admission"))
	}
	return nil
}
