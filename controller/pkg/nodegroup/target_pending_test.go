package nodegroup

import (
	"context"
	"errors"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPendingDrainRetainsMachineIdentity(t *testing.T) {
	for _, version := range []string{"v1beta1", "v1beta2"} {
		t.Run(version, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = v1alpha1.AddToScheme(scheme)
			_ = corev1.AddToScheme(scheme)
			api := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.ProvisionedNodeGroupClaim{}).Build()
			verifyPendingDrainIdentity(t, api, schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: version, Kind: "Machine"})
		})
	}
}

func verifyPendingDrainIdentity(t *testing.T, api client.Client, gvk schema.GroupVersionKind) {
	t.Helper()
	for _, scenario := range []string{"late-node", "late-provider", "referenced-node-absent", "replacement-machine", "changed-provider", "stale-group", "wrong-node-uid"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			g := groupFixture()
			g.Name = "pending-" + scenario
			g.Namespace = "default"
			g.ResourceVersion = ""
			g.Generation = 1
			g.UID = types.UID("group-" + scenario)
			zero := int32(0)
			g.Spec.Replicas = &zero
			if err := api.Create(ctx, g); err != nil {
				t.Fatal(err)
			}
			child, err := BuildChild(g, 0)
			if err != nil {
				t.Fatal(err)
			}
			child.UID = types.UID("child-" + scenario)
			if err = api.Create(ctx, child); err != nil {
				t.Fatal(err)
			}
			action, err := ProposeAction(g, []v1alpha1.ProvisionedNodeClaim{*child})
			if err != nil {
				t.Fatal(err)
			}
			g, err = ReserveAction(ctx, api, g, action)
			if err != nil {
				t.Fatal(err)
			}
			provider := "test:///" + scenario
			machine := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"bootstrap": map[string]interface{}{"dataSecretName": child.Name + "-bootstrap"}}}}
			machine.SetGroupVersionKind(gvk)
			machine.SetName(child.Name)
			machine.SetNamespace(child.Namespace)
			machine.SetUID(types.UID("machine-" + scenario))
			machine.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(child, v1alpha1.GroupVersion.WithKind("ProvisionedNodeClaim"))})
			if scenario != "late-provider" {
				_ = unstructured.SetNestedField(machine.Object, provider, "spec", "providerID")
			}
			if scenario == "referenced-node-absent" {
				_ = unstructured.SetNestedField(machine.Object, g.Name, "status", "nodeRef", "name")
			}
			if err = api.Create(ctx, machine); err != nil {
				t.Fatal(err)
			}
			originalUID := machine.GetUID()
			if scenario == "stale-group" {
				newer := g.DeepCopy()
				newer.Labels = map[string]string{"concurrent": "update"}
				if err = api.Update(ctx, newer); err != nil {
					t.Fatal(err)
				}
				if _, err = BindDrainNode(ctx, api, api, gvk, g); !apierrors.IsConflict(err) {
					t.Fatal("stale journal write accepted", err)
				}
				if err = api.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
					t.Fatal(err)
				}
				if g.Status.PendingAction.MachineUID != "" {
					t.Fatal("stale journal mutated target")
				}
			}
			bound, err := BindDrainNode(ctx, api, api, gvk, g)
			if !errors.Is(err, errUnresolvedNode) || bound == nil {
				t.Fatal("pending target not recorded", err)
			}
			if bound.Status.PendingAction.MachineUID != string(originalUID) || bound.Status.PendingAction.NodeUID != "" {
				t.Fatal("incorrect partial target")
			}
			if g.Status.PendingAction.MachineUID != "" {
				t.Fatal("caller mutated")
			}
			if err = api.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
				t.Fatal(err)
			}
			rv := g.ResourceVersion
			if _, err = BindDrainNode(ctx, api, api, gvk, g); !errors.Is(err, errUnresolvedNode) {
				t.Fatal(err)
			}
			if err = api.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
				t.Fatal(err)
			}
			if g.ResourceVersion != rv {
				t.Fatal("unchanged pending target rewrote status")
			}
			if scenario == "replacement-machine" {
				if err = api.Delete(ctx, machine); err != nil {
					t.Fatal(err)
				}
				machine.SetResourceVersion("")
				machine.SetUID("replacement")
				if err = api.Create(ctx, machine); err != nil {
					t.Fatal(err)
				}
				if _, err = BindDrainNode(ctx, api, api, gvk, g); err == nil || errors.Is(err, errUnresolvedNode) {
					t.Fatal("replacement treated as original pending compute", err)
				}
			} else if scenario == "changed-provider" {
				_ = unstructured.SetNestedField(machine.Object, "test:///replacement", "spec", "providerID")
				if err = api.Update(ctx, machine); err != nil {
					t.Fatal(err)
				}
				if _, err = BindDrainNode(ctx, api, api, gvk, g); err == nil || errors.Is(err, errUnresolvedNode) {
					t.Fatal("changed provider identity accepted", err)
				}
			} else {
				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: g.Name, UID: types.UID("node-" + scenario)}, Spec: corev1.NodeSpec{ProviderID: provider}}
				if err = api.Create(ctx, node); err != nil {
					t.Fatal(err)
				}
				_ = unstructured.SetNestedField(machine.Object, provider, "spec", "providerID")
				_ = unstructured.SetNestedField(machine.Object, node.Name, "status", "nodeRef", "name")
				uid := string(node.UID)
				if scenario == "wrong-node-uid" {
					uid = "old-node"
				}
				_ = unstructured.SetNestedField(machine.Object, uid, "status", "nodeRef", "uid")
				if err = api.Update(ctx, machine); err != nil {
					t.Fatal(err)
				}
				bound, err = BindDrainNode(ctx, api, api, gvk, g)
				if scenario == "wrong-node-uid" {
					if err == nil {
						t.Fatal("stale nodeRef UID accepted")
					}
				} else {
					if err != nil {
						t.Fatal("late registration did not bind", err)
					}
					if bound.Status.PendingAction.MachineUID != string(originalUID) || bound.Status.PendingAction.NodeUID != string(node.UID) || bound.Status.PendingAction.ProviderID != provider {
						t.Fatal("late registration changed identity")
					}
				}
			}
			if err = api.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
				t.Fatal(err)
			}
			if g.Status.PendingAction.MachineUID != string(originalUID) {
				t.Fatal("recorded Machine UID changed")
			}
			if err = api.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil || !child.DeletionTimestamp.IsZero() {
				t.Fatal("partial target bypassed removal gates", err)
			}
		})
	}
}
