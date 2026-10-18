package attachment

import (
	"context"
	"fmt"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Synthetic terminal history and API objects test capture only, not live
// cleanup, cross-object atomicity, retirement receipts or traffic continuity.
func TestOrphanReceiveWithdrawalCapture(t *testing.T) {
	for _, fault := range []string{"none", "machine", "node", "adoption", "read-error", "unfrozen", "unfinished", "republished", "missing-site"} {
		t.Run(fault, func(t *testing.T) {
			outcome := "forward"
			if fault == "unfinished" {
				outcome = "unfinished"
			}
			_, snapshot, inventory, mesh := retirementFixture(t, outcome, fault != "unfrozen")
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			gvk := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}
			scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
			scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("MachineList"), &unstructured.UnstructuredList{})
			objects := []client.Object{mesh, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "site", UID: "site-uid"}}}
			switch fault {
			case "machine":
				m := &unstructured.Unstructured{}
				m.SetGroupVersionKind(gvk)
				m.SetName("worker")
				m.SetNamespace(mesh.Namespace)
				m.SetUID("replacement")
				objects = append(objects, m)
			case "node":
				objects = append(objects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cloud", UID: "replacement"}})
			case "adoption":
				objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName("worker"), Namespace: mesh.Namespace, UID: "replacement"}})
			case "republished":
				plan, err := tunnel.DecodeReceiveTransition(snapshot, inventory.ClusterUID)
				if err != nil {
					t.Fatal(err)
				}
				mesh.Data[tunnel.PeerPublicKeyPrefix+"worker"] = []byte(plan.RootPublicKey)
			case "missing-site":
				objects = objects[:1]
			}
			api := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			var reader client.Reader = api
			if fault == "read-error" {
				reader = orphanReadFailure{Reader: api}
			}
			resolver := MeshConsumerResolver{Reader: reader, Namespace: mesh.Namespace, SecretName: mesh.Name, SecretUID: string(mesh.UID)}
			intent, err := resolver.PrepareOrphanReceiveWithdrawal(context.Background(), snapshot, inventory, mesh)
			if fault != "none" {
				if err == nil {
					t.Fatal("unsafe orphan capture accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !intent.Orphaned || len(intent.Consumers) != 1 || intent.Consumers[0].NodeUID != "site-uid" {
				t.Fatalf("lost survivor: %+v", intent)
			}
			if _, err := intent.ReceiveRetirementPlan(snapshot, inventory, mesh); err != nil {
				t.Fatal(err)
			}
			for _, change := range []string{"source-data", "journal-marker", "missing-hash", "survivor"} {
				t.Run(change, func(t *testing.T) {
					copy := *intent
					changed := mesh.DeepCopy()
					switch change {
					case "source-data":
						changed.Data["changed"] = []byte("publication")
					case "journal-marker":
						copy.DrainIntent = "orphan:replacement"
					case "missing-hash":
						copy.SourceDataHash = ""
					case "survivor":
						copy.Consumers = nil
					}
					if _, err := copy.ReceiveRetirementPlan(snapshot, inventory, changed); err == nil {
						t.Fatal("changed orphan authority accepted")
					}
				})
			}
		})
	}
}

type orphanReadFailure struct{ client.Reader }

func (r orphanReadFailure) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*unstructured.Unstructured); ok {
		return fmt.Errorf("API unavailable")
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}
