package nodegroup

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	claimcontroller "github.com/appmana/cloud-provisioning/controller/pkg/claim"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func bootstrapCancellationFixture(t *testing.T, api client.Client, name string) (*Reconciler, *v1alpha1.ProvisionedNodeGroupClaim, *v1alpha1.ProvisionedNodeClaim, *unstructured.Unstructured) {
	t.Helper()
	ctx := context.Background()
	g := cancellationFixture(t, api, name)
	child, err := ResumeCreation(ctx, api, client.ObjectKeyFromObject(g), g.UID, g.Status.PendingAction.ID)
	if err != nil {
		t.Fatal(err)
	}
	child.Finalizers = []string{claimcontroller.Finalizer}
	if err = api.Update(ctx, child); err != nil {
		t.Fatal(err)
	}
	g, err = CompleteCreation(ctx, api, g, child.UID)
	if err != nil {
		t.Fatal(err)
	}
	zero := int32(0)
	g.Spec.Replicas = &zero
	if err = api.Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	a, err := ProposeAction(g, []v1alpha1.ProvisionedNodeClaim{*child})
	if err != nil {
		t.Fatal(err)
	}
	g, err = ReserveAction(ctx, api, g, a)
	if err != nil {
		t.Fatal(err)
	}
	gvk := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}
	m := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"bootstrap": map[string]interface{}{"dataSecretName": child.Name + "-bootstrap"}}}}
	m.SetGroupVersionKind(gvk)
	m.SetName(child.Name)
	m.SetNamespace(child.Namespace)
	m.SetFinalizers([]string{"test/compute"})
	m.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(child, v1alpha1.GroupVersion.WithKind("ProvisionedNodeClaim"))})
	m.SetAnnotations(map[string]string{"pre-terminate.delete.hook.machine.cluster.x-k8s.io/other": "other-owner"})
	if err = api.Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	g, err = BindDrainNode(ctx, api, api, gvk, g)
	if !errors.Is(err, errUnresolvedNode) {
		t.Fatal(err)
	}
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-mesh", Namespace: g.Namespace}, Data: map[string][]byte{}}
	if err = api.Create(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	return &Reconciler{API: api, Workload: api, MachineGVK: gvk, MeshName: mesh.Name, BootstrapSecretNameFormat: "%s-bootstrap"}, g, child, m
}

func verifyBootstrapCancellation(t *testing.T, api client.Client) {
	t.Helper()
	ctx := context.Background()
	t.Run("cancel-before-bootstrap-lifecycle", func(t *testing.T) {
		r, g, child, m := bootstrapCancellationFixture(t, api, "cancel-bootstrap-lifecycle")
		staleMachine := m.DeepCopy()
		if changed, err := r.beginBootstrapCancellation(ctx, g); err != nil || !changed {
			t.Fatal("marker not installed", changed, err)
		}
		if err := api.Get(ctx, client.ObjectKeyFromObject(m), m); err != nil {
			t.Fatal(err)
		}
		want := string(g.UID) + "/" + g.Status.PendingAction.ID
		if m.GetAnnotations()[BootstrapCancellationAnnotation] != want || m.GetAnnotations()[attachment.DrainIntentAnnotation] != want || m.GetAnnotations()["pre-terminate.delete.hook.machine.cluster.x-k8s.io/other"] != "other-owner" {
			t.Fatal("incorrect marker or unrelated hook changed")
		}
		// Execute the real join entry point: the marker must prevent rendering or
		// creating userdata even though the Machine already references its Secret.
		jr := &join.Reconciler{Client: api, Reader: api}
		if _, err := jr.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}); err != nil {
			t.Fatal(err)
		}
		before := staleMachine.DeepCopy()
		annotations := staleMachine.GetAnnotations()
		annotations[join.WireGuardAddrAnnotation] = "10.100.0.99/24"
		staleMachine.SetAnnotations(annotations)
		if err := api.Patch(ctx, staleMachine, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); !apierrors.IsConflict(err) {
			t.Fatal("in-flight reservation crossed cancellation", err)
		}
		if changed, err := r.beginBootstrapCancellation(ctx, g); err != nil || !changed {
			t.Fatal("cancellation not journaled", changed, err)
		}
		if err := api.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
			t.Fatal(err)
		}
		if g.Status.PendingAction.BootstrapCancellation == nil {
			t.Fatal("API pruned cancellation")
		}
		copied := g.DeepCopy()
		copied.Status.PendingAction.BootstrapCancellation.SecretName = "changed"
		if reflect.DeepEqual(copied.Status.PendingAction.BootstrapCancellation, g.Status.PendingAction.BootstrapCancellation) {
			t.Fatal("cancellation proof aliased")
		}
		// A provider can assign an instance identity while the claim is pending.
		// Once bootstrap is fenced, that metadata must not prevent CAPI teardown.
		_ = unstructured.SetNestedField(m.Object, "test:///allocated-after-fence", "spec", "providerID")
		if err := api.Update(ctx, m); err != nil {
			t.Fatal(err)
		}
		if done, err := r.resumeBootstrapCancellation(ctx, g); err != nil || done {
			t.Fatal("delete request completed cancellation", done, err)
		}
		if err := api.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil || child.DeletionTimestamp.IsZero() {
			t.Fatal("claim not deleting", err)
		}
		cr := &claimcontroller.Reconciler{Client: api, Reader: api}
		if _, err := cr.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(child)}); err != nil {
			t.Fatal(err)
		}
		if err := api.Get(ctx, client.ObjectKeyFromObject(m), m); err != nil || m.GetDeletionTimestamp().IsZero() {
			t.Fatal("CAPI deletion not requested", err)
		}
		if len(m.GetFinalizers()) != 1 || m.GetAnnotations()["pre-terminate.delete.hook.machine.cluster.x-k8s.io/other"] != "other-owner" {
			t.Fatal("CAPI lifetime bypassed")
		}
		if done, err := r.resumeBootstrapCancellation(ctx, g); err != nil || done {
			t.Fatal("live resources skipped", done, err)
		}
		// Envtest has no infrastructure provider. Release its simulated compute
		// finalizer explicitly; native validation must exercise the real provider.
		m.SetFinalizers(nil)
		if err := api.Update(ctx, m); err != nil {
			t.Fatal(err)
		}
		if _, err := cr.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(child)}); err != nil {
			t.Fatal(err)
		}
		if done, err := r.resumeBootstrapCancellation(ctx, g); err != nil || !done {
			t.Fatal("absent resources did not finish", done, err)
		}
		if err := api.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
			t.Fatal(err)
		}
		if g.Status.PendingAction != nil {
			t.Fatal("cancellation action retained")
		}
	})
	for _, scenario := range []string{"reservation-wins", "userdata-present", "peer-present", "node-present", "secret-after-fence", "marker-replaced"} {
		t.Run(scenario, func(t *testing.T) {
			r, g, child, m := bootstrapCancellationFixture(t, api, "cancel-bootstrap-"+scenario)
			if scenario == "reservation-wins" {
				racer := &bootstrapPatchRaceClient{Client: api, before: func() {
					current := m.DeepCopy()
					a := current.GetAnnotations()
					a[join.WireGuardAddrAnnotation] = "10.100.0.99/24"
					current.SetAnnotations(a)
					if err := api.Update(ctx, current); err != nil {
						t.Fatal(err)
					}
				}}
				r.API = racer
				if changed, err := r.beginBootstrapCancellation(ctx, g); !apierrors.IsConflict(err) || changed {
					t.Fatal("cancellation crossed reservation", changed, err)
				}
				r.API = api
			} else if scenario == "peer-present" {
				mesh := &corev1.Secret{}
				if err := api.Get(ctx, client.ObjectKey{Namespace: g.Namespace, Name: r.MeshName}, mesh); err != nil {
					t.Fatal(err)
				}
				mesh.Data = map[string][]byte{tunnel.PeerEndpointPrefix + child.Name: []byte("pending")}
				if err := api.Update(ctx, mesh); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "node-present" {
				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: g.Name, Annotations: map[string]string{claimcontroller.ClaimAnnotation: g.Namespace + "/" + child.Name}}}
				if err := api.Create(ctx, node); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "userdata-present" {
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: child.Name + "-bootstrap", Namespace: g.Namespace}}
				if err := api.Create(ctx, secret); err != nil {
					t.Fatal(err)
				}
			} else {
				for i := 0; i < 2; i++ {
					if changed, err := r.beginBootstrapCancellation(ctx, g); err != nil || !changed {
						t.Fatal(err)
					}
				}
				if err := api.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
					t.Fatal(err)
				}
				if scenario == "secret-after-fence" {
					secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: child.Name + "-bootstrap", Namespace: g.Namespace}}
					if err := api.Create(ctx, secret); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := api.Get(ctx, client.ObjectKeyFromObject(m), m); err != nil {
						t.Fatal(err)
					}
					a := m.GetAnnotations()
					a[BootstrapCancellationAnnotation] = "other"
					m.SetAnnotations(a)
					if err := api.Update(ctx, m); err != nil {
						t.Fatal(err)
					}
				}
				if done, err := r.resumeBootstrapCancellation(ctx, g); err == nil || done {
					t.Fatal("changed cancellation proof accepted", done, err)
				}
			}
			if g.Status.PendingAction.BootstrapCancellation == nil {
				if changed, err := r.beginBootstrapCancellation(ctx, g); err != nil || changed {
					t.Fatal("started worker cancelled", changed, err)
				}
			}
			if err := api.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil || !child.DeletionTimestamp.IsZero() {
				t.Fatal("claim deleted across hold", err)
			}
		})
	}
}

type bootstrapPatchRaceClient struct {
	client.Client
	before func()
}

func (c *bootstrapPatchRaceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.before()
	return c.Client.Patch(ctx, obj, patch, opts...)
}
