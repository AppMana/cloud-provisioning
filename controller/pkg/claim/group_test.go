package claim

import (
	"context"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGroupLifetimeGuardsClaimAdmission(t *testing.T) {
	for _, state := range []string{"active", "deleting", "missing", "replaced", "ambiguous", "wrong-version", "admitted-deleting", "admitted-missing", "admitted-replaced"} {
		t.Run(state, func(t *testing.T) {
			claim := fakeClaim("group-worker")
			parent := &v1alpha1.ProvisionedNodeGroupClaim{ObjectMeta: metav1.ObjectMeta{Name: "workers", Namespace: claim.Namespace, UID: "group", Finalizers: []string{"test/hold"}}}
			owner := metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ProvisionedNodeGroupClaim", Name: parent.Name, UID: parent.UID}
			claim.OwnerReferences = []metav1.OwnerReference{owner}
			if state == "deleting" || state == "admitted-deleting" {
				now := metav1.Now()
				parent.DeletionTimestamp = &now
			}
			admitted := state == "admitted-deleting" || state == "admitted-missing" || state == "admitted-replaced"
			if admitted {
				claim.Finalizers = []string{Finalizer}
			}
			if state == "replaced" || state == "admitted-replaced" {
				parent.UID = "replacement"
			}
			if state == "ambiguous" {
				other := owner
				other.UID = "other-group"
				other.Name = "other"
				claim.OwnerReferences = append(claim.OwnerReferences, other)
			}
			if state == "wrong-version" {
				claim.OwnerReferences[0].APIVersion = "example.invalid/v1"
			}
			objects := []client.Object{claim, fakeTemplate(claim.Name), fakeCluster("appmana"), awsConfigSecret(), fakeNode()}
			if state != "missing" && state != "admitted-missing" {
				objects = append(objects, parent)
			}
			r := newClaimReconciler(t, objects...)
			err := reconcileClaim(t, r, claim)
			if (err != nil) != (state == "ambiguous" || state == "wrong-version") {
				t.Fatalf("state %s: %v", state, err)
			}
			current := &v1alpha1.ProvisionedNodeClaim{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), current); err != nil {
				t.Fatal(err)
			}
			if containsString(current.Finalizers, Finalizer) != (state == "active" || admitted) {
				t.Fatal("incorrect admission boundary", current.Finalizers)
			}
			machine := &unstructured.Unstructured{}
			machine.SetGroupVersionKind(machineGVK)
			infra := &unstructured.Unstructured{}
			infra.SetGroupVersionKind(joinaws.Provider{}.GVK())
			for _, obj := range []*unstructured.Unstructured{machine, infra} {
				err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), obj)
				if state == "active" || admitted {
					if err != nil {
						t.Fatal(err)
					}
				} else if !apierrors.IsNotFound(err) {
					t.Fatal("retired group created compute", obj.GetKind(), err)
				}
			}
		})
	}
}

// Group authorization is a durable lifecycle decision, not a fresh cleanup
// receipt. This fake-API check does not qualify native retirement or CAPI drain.
func TestGroupClaimDeletionRequiresMatchingCommittedAction(t *testing.T) {
	for _, state := range []string{"committed", "pending", "group-uid", "child-uid", "machine-uid", "marker", "provider", "mesh", "cancellation", "invalid-cancellation"} {
		t.Run(state, func(t *testing.T) {
			claim := fakeClaim("worker")
			claim.UID = "claim"
			owner := metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ProvisionedNodeGroupClaim", Name: "workers", UID: "group"}
			claim.OwnerReferences = []metav1.OwnerReference{owner}
			group := &v1alpha1.ProvisionedNodeGroupClaim{ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: claim.Namespace, UID: owner.UID}, Status: v1alpha1.ProvisionedNodeGroupClaimStatus{PendingAction: &v1alpha1.NodeGroupAction{Type: "Drain", ID: "remove", ChildName: claim.Name, ChildUID: string(claim.UID), MachineUID: "machine", NodeName: "worker", NodeUID: "node", ProviderID: "test:///worker", Removing: true, Withdrawal: &v1alpha1.GroupPeerWithdrawal{MeshName: "mesh"}}}}
			machine := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"providerID": "test:///worker"}, "status": map[string]any{"nodeRef": map[string]any{"name": "worker", "uid": "node"}}}}
			machine.SetGroupVersionKind(machineGVK)
			machine.SetName(claim.Name)
			machine.SetNamespace(claim.Namespace)
			machine.SetUID("machine")
			annotations := map[string]string{ClaimAnnotation: claim.Namespace + "/" + claim.Name, ClaimUIDAnnotation: string(claim.UID), "cloud-provisioning.appmana.com/drain-intent": "group/remove"}
			switch state {
			case "pending":
				group.Status.PendingAction.Removing = false
			case "group-uid":
				group.UID = "replacement"
			case "child-uid":
				group.Status.PendingAction.ChildUID = "other"
			case "machine-uid":
				machine.SetUID("other")
			case "marker":
				annotations["cloud-provisioning.appmana.com/drain-intent"] = "other"
			case "provider":
				group.Status.PendingAction.ProviderID = "test:///other"
			case "mesh":
				group.Status.PendingAction.Withdrawal.MeshName = "other"
			case "cancellation", "invalid-cancellation":
				group.Status.PendingAction.Removing = false
				group.Status.PendingAction.Withdrawal = nil
				group.Status.PendingAction.NodeName = ""
				group.Status.PendingAction.NodeUID = ""
				unstructured.RemoveNestedField(machine.Object, "status", "nodeRef")
				_ = unstructured.SetNestedField(machine.Object, "bootstrap", "spec", "bootstrap", "dataSecretName")
				group.Status.PendingAction.BootstrapCancellation = &v1alpha1.BootstrapCancellation{SecretName: "bootstrap", MeshName: "mesh", MeshUID: "mesh-uid"}
				if state == "cancellation" {
					annotations["cloud-provisioning.appmana.com/cancel-before-bootstrap"] = "group/remove"
				}
			}
			machine.SetAnnotations(annotations)
			r := newClaimReconciler(t, claim, group, machine)
			r.MeshName = "mesh"
			allowed, err := r.groupReceiveDeletionCommitted(context.Background(), claim, machine, owner)
			want := state == "committed" || state == "cancellation"
			if allowed != want || (want && err != nil) {
				t.Fatal("incorrect group deletion admission", allowed, err)
			}
		})
	}
}
