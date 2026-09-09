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
