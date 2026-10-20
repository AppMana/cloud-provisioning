package attachment

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PrepareOrphanReceiveWithdrawal captures surviving consumers when an older
// deletion already removed the Machine, Node, adoption Secret and publication,
// but retained its journal. The caller must authenticate journal history and
// live inventory revisions, freeze completion, and persist the returned intent
// through ReserveReceiveWithdrawal. This read-only capture neither releases
// reservations nor proves native cleanup. Reader must bypass informer caches.
func (r MeshConsumerResolver) PrepareOrphanReceiveWithdrawal(ctx context.Context, snapshot handover.Snapshot, inventory tunnel.ReceiveInventory, mesh *corev1.Secret) (*RemoteWithdrawalIntent, error) {
	plan, err := tunnel.DecodeReceiveTransition(snapshot, inventory.ClusterUID)
	if err != nil {
		return nil, err
	}
	if err := r.CheckReceiveOrphanAbsent(ctx, plan); err != nil {
		return nil, err
	}
	consumers, err := r.PublishedConsumers(ctx, mesh)
	if err != nil {
		return nil, err
	}
	w := &RemoteWithdrawalIntent{MeshNamespace: mesh.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), SourceVersion: mesh.ResourceVersion, SourceDataHash: withdrawalSourceDataHash(mesh.Data), Orphaned: true, MachineName: plan.MachineName, MachineUID: plan.MachineUID, PublicKey: plan.RootPublicKey, DrainIntent: "orphan:" + string(snapshot.Metadata().UID), Consumers: consumers}
	if _, err := w.receiveRetirementPlan(snapshot, inventory, mesh, false); err != nil {
		return nil, err
	}
	if err := r.CheckReceiveOrphanAbsent(ctx, plan); err != nil {
		return nil, err
	}
	return w, nil
}

// CheckReceiveOrphanAbsent rejects read failures and replacement objects as well
// as the original owner. Absence alone is not withdrawal authority: callers also
// bind the exact reserved terminal history, persisted intent and current mesh.
// Repeat these reads immediately before publishing or advancing retirement.
func (r MeshConsumerResolver) CheckReceiveOrphanAbsent(ctx context.Context, plan tunnel.ReceiveTransitionPlan) error {
	absent, err := r.ReceiveOwnerAbsent(ctx, plan)
	if err != nil {
		return err
	}
	if !absent {
		return fmt.Errorf("retirement owner or replacement still exists")
	}
	return nil
}

// ReceiveOwnerAbsent distinguishes unfinished deletion from a failed read.
// Existing objects, including replacements, keep ownership reserved. Callers
// must still authorize withdrawal history and repeat absence checks at writes;
// this read is not a provider deletion or native cleanup proof.
func (r MeshConsumerResolver) ReceiveOwnerAbsent(ctx context.Context, plan tunnel.ReceiveTransitionPlan) (bool, error) {
	if r.Reader == nil || r.Namespace == "" || r.SecretName == "" || r.SecretUID == "" || plan.Namespace != r.Namespace || plan.MeshName != r.SecretName || plan.MeshUID != r.SecretUID || plan.MachineName == "" || plan.MachineUID == "" || plan.CloudNodeName == "" || plan.CloudNodeUID == "" || plan.AdoptionName != tunnel.AdoptionSecretName(plan.MachineName) || plan.AdoptionUID == "" {
		return false, fmt.Errorf("retirement absence requires exact reserved identities")
	}
	absent := true
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"})
	for _, target := range []struct {
		key client.ObjectKey
		obj client.Object
	}{
		{client.ObjectKey{Namespace: r.Namespace, Name: plan.MachineName}, machine},
		{client.ObjectKey{Name: plan.CloudNodeName}, &corev1.Node{}},
		{client.ObjectKey{Namespace: r.Namespace, Name: plan.AdoptionName}, &corev1.Secret{}},
	} {
		err := r.Reader.Get(ctx, target.key, target.obj)
		if err == nil {
			absent = false
			continue
		}
		if !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return absent, ctx.Err()
}
