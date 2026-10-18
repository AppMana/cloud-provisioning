package attachment

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// NewReceiveRetirementReceiptCollection connects the bounded receipt transport
// to revision-bound completion storage. Call from trusted live admission on each
// request; recheck must admit the full journal/mesh/consumer source set. Policies
// and maxAge are controller settings, never agent-supplied. Collection does not
// release reservations or substitute for each agent's native absence check.
func NewReceiveRetirementReceiptCollection(ctx context.Context, api kubernetes.Interface, store handover.Store, inventoryUID string, mesh *corev1.Secret, snapshot handover.Snapshot, policies map[string]handover.PodReceiptAuthenticator, maxAge time.Duration, recheck func() error) (handover.ReceiptCollection, error) {
	fail := func(err error) (handover.ReceiptCollection, error) { return handover.ReceiptCollection{}, err }
	if api == nil || store.ConfigMaps == nil || mesh == nil || recheck == nil || maxAge <= 0 {
		return fail(fmt.Errorf("retirement collection requires live admission and bounded observation age"))
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	if err != nil {
		return fail(err)
	}
	shared, err := ReadReceiveRetirementSet(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), plan.MachineUID)
	if err != nil {
		return fail(err)
	}
	if string(shared.Metadata.UID) != inventoryUID || shared.MeshMetadata.ResourceVersion != mesh.ResourceVersion {
		return fail(fmt.Errorf("retirement collection source changed"))
	}
	if err := shared.Journals[plan.MachineUID].ContinuesExecution(snapshot); err != nil {
		return fail(err)
	}
	return shared.ReceiptCollection(ctx, api, store, policies, maxAge, recheck)
}

// ReceiptCollection uses this request's trusted, fully admitted source. The
// callback rechecks consumer policy and owner absence; source rechecks remain
// part of the collection. Never reuse admission across requests. Obtain this
// source through ReadReceiveRetirementSet, not a manually assembled value.
func (shared ReceiveRetirementSet) ReceiptCollection(ctx context.Context, api kubernetes.Interface, store handover.Store, policies map[string]handover.PodReceiptAuthenticator, maxAge time.Duration, recheck func() error) (handover.ReceiptCollection, error) {
	fail := func(err error) (handover.ReceiptCollection, error) { return handover.ReceiptCollection{}, err }
	if api == nil || store.ConfigMaps == nil || shared.Mesh == nil || recheck == nil || maxAge <= 0 {
		return fail(fmt.Errorf("retirement collection requires admitted sources and bounded observation age"))
	}
	machineUID := shared.Withdrawal.MachineUID
	snapshot, found := shared.Journals[machineUID]
	if !found || store.Name != tunnel.ReceiveTransitionName(machineUID) || (store.ExpectedUID != "" && store.ExpectedUID != string(snapshot.Metadata().UID)) || shared.Inventory.ClusterUID != store.ClusterUID {
		return fail(fmt.Errorf("retirement collection journal scope changed"))
	}
	mesh, inventoryUID := shared.Mesh, string(shared.Metadata.UID)
	if shared.MeshMetadata.UID != mesh.UID || shared.MeshMetadata.ResourceVersion != mesh.ResourceVersion || inventoryUID == "" || snapshot.Metadata().Namespace != mesh.Namespace {
		return fail(fmt.Errorf("retirement collection source scope changed"))
	}
	withdrawal, err := ReadReceiveWithdrawal(shared.Inventory, machineUID, snapshot, mesh)
	if err != nil {
		return fail(err)
	}
	if !reflect.DeepEqual(withdrawal, shared.Withdrawal) {
		return fail(fmt.Errorf("retirement collection withdrawal changed"))
	}
	check := func(ctx context.Context) error {
		if err := recheck(); err != nil {
			return err
		}
		return shared.Recheck(ctx, api, store)
	}
	if err := check(ctx); err != nil {
		return fail(err)
	}
	scope, err := NewReceiveRetirementReceiptScope(ctx, store.ConfigMaps, store.ClusterUID, inventoryUID, snapshot, mesh)
	if err != nil {
		return fail(err)
	}
	target := handover.ReceiptCollection{UID: string(snapshot.Metadata().UID), Digest: scope.Digest(), ExpiresAt: time.Now().Add(maxAge), MaxAge: maxAge, ReadOnlyObserve: true}
	for uid := range scope.nodes {
		if _, ok := policies[uid]; !ok {
			return fail(fmt.Errorf("retirement consumer has no trusted agent policy"))
		}
		target.Members = append(target.Members, handover.ReceiptMember{NodeUID: uid})
	}
	sort.Slice(target.Members, func(i, j int) bool { return target.Members[i].NodeUID < target.Members[j].NodeUID })
	target.Observe = func(ctx context.Context, entries []handover.PodReceipt) error {
		if err := check(ctx); err != nil {
			return err
		}
		if _, err := scope.AuthenticatePartial(ctx, entries, policies, maxAge); err != nil {
			return err
		}
		if err := check(ctx); err != nil {
			return err
		}
		current, err := NewReceiveRetirementReceiptScope(ctx, store.ConfigMaps, store.ClusterUID, inventoryUID, snapshot, mesh)
		if err != nil {
			return err
		}
		if current.Digest() != scope.Digest() {
			return fmt.Errorf("retirement source changed during collection")
		}
		return scope.validateBatch(entries, time.Now(), maxAge, false)
	}
	target.Complete = func(ctx context.Context, entries []handover.PodReceipt) error {
		return RecordReceiveRetirementWithPodReceipts(ctx, api, store, inventoryUID, scope.Digest(), mesh, snapshot, entries, policies, maxAge, func() error {
			return check(ctx)
		})
	}
	return target, nil
}
