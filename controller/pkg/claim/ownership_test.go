package claim

import (
	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

// Association checks only. This does not authorize deletion, authenticate
// metadata writers, backfill unbound legacy objects or qualify live CAPI races.
func TestMachineClaimAssociationSurvivesOwnerChanges(t *testing.T) {
	for _, state := range []string{"bound", "cluster-owner", "legacy", "absent", "partial-name", "partial-uid", "wrong-uid", "wrong-name", "wrong-namespace", "conflicting-owner", "empty-claim-uid"} {
		t.Run(state, func(t *testing.T) {
			child := fakeClaim("worker")
			child.UID = "claim-uid"
			machine := &unstructured.Unstructured{}
			machine.SetName(child.Name)
			machine.SetNamespace(child.Namespace)
			annotations := map[string]string{ClaimAnnotation: child.Namespace + "/" + child.Name, ClaimUIDAnnotation: string(child.UID)}
			machine.SetAnnotations(annotations)
			switch state {
			case "cluster-owner":
				machine.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "cluster.x-k8s.io/v1beta2", Kind: "Cluster", Name: "site", UID: "cluster"}})
			case "legacy":
				machine.SetAnnotations(nil)
				machine.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ProvisionedNodeClaim", Name: child.Name, UID: child.UID}})
			case "absent":
				machine.SetAnnotations(nil)
			case "partial-name":
				delete(annotations, ClaimUIDAnnotation)
				machine.SetAnnotations(annotations)
			case "partial-uid":
				delete(annotations, ClaimAnnotation)
				machine.SetAnnotations(annotations)
			case "wrong-uid":
				annotations[ClaimUIDAnnotation] = "other"
				machine.SetAnnotations(annotations)
			case "wrong-name":
				machine.SetName("other")
			case "wrong-namespace":
				machine.SetNamespace("other")
			case "conflicting-owner":
				machine.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ProvisionedNodeClaim", Name: child.Name, UID: "other"}})
			case "empty-claim-uid":
				child.UID = ""
			}
			want := state == "bound" || state == "cluster-owner" || state == "legacy"
			if MachineOwnedByClaim(machine, child) != want {
				t.Fatal("incorrect claim association")
			}
		})
	}
}
