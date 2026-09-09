package nodegroup

import (
	"context"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGroupNodeLabelRequiresOwnedAssociation(t *testing.T) {
	for _, version := range []string{"v1beta1", "v1beta2"} {
		for _, scenario := range []string{"linux", "windows", "wrong-owner", "wrong-provider", "wrong-node", "unallocated", "draining", "another-group", "node-race", "machine-race"} {
			t.Run(version+"/"+scenario, func(t *testing.T) {
				ctx := context.Background()
				group := groupFixture()
				child, err := BuildChild(group, 0)
				if err != nil {
					t.Fatal(err)
				}
				child.UID = "child"
				gvk := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: version, Kind: "Machine"}
				machine := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"providerID": "test:///worker"}, "status": map[string]interface{}{"nodeRef": map[string]interface{}{"name": "worker", "uid": "node"}}}}
				machine.SetGroupVersionKind(gvk)
				machine.SetName(child.Name)
				machine.SetNamespace(child.Namespace)
				machine.SetUID("machine")
				machine.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(child, v1alpha1.GroupVersion.WithKind("ProvisionedNodeClaim"))})
				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node", Labels: map[string]string{"unrelated": "keep", corev1.LabelOSStable: scenario}}, Spec: corev1.NodeSpec{ProviderID: "test:///worker"}}
				switch scenario {
				case "wrong-owner":
					machine.SetOwnerReferences(nil)
				case "wrong-provider":
					node.Spec.ProviderID = "other:///node"
				case "wrong-node":
					node.UID = "replacement"
				case "unallocated":
					unstructured.RemoveNestedField(machine.Object, "status", "nodeRef")
				case "draining":
					group.Status.PendingAction = &v1alpha1.NodeGroupAction{Type: DrainAction, ChildUID: string(child.UID)}
				case "another-group":
					node.Labels[GroupUIDLabel] = "other-group"
				}
				scheme := runtime.NewScheme()
				_ = corev1.AddToScheme(scheme)
				_ = v1alpha1.AddToScheme(scheme)
				api := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group, child, machine).Build()
				workload := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
				// Use API-returned resource versions, as the uncached reconciler does.
				if err := api.Get(ctx, client.ObjectKeyFromObject(group), group); err != nil {
					t.Fatal(err)
				}
				if err := api.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil {
					t.Fatal(err)
				}
				r := &Reconciler{API: api, Workload: workload, MachineGVK: gvk}
				if scenario == "node-race" {
					r.Workload = &labelRaceClient{Client: workload, mutate: func() {
						current := &corev1.Node{}
						if err := workload.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
							t.Fatal(err)
						}
						current.Labels[GroupUIDLabel] = "concurrent-owner"
						if err := workload.Update(ctx, current); err != nil {
							t.Fatal(err)
						}
					}}
				}
				if scenario == "machine-race" {
					r.API = &readinessRaceClient{Client: api, key: client.ObjectKeyFromObject(machine), mutate: func() {
						current := machine.DeepCopy()
						if err := api.Get(ctx, client.ObjectKeyFromObject(machine), current); err != nil {
							t.Fatal(err)
						}
						current.SetOwnerReferences(nil)
						if err := api.Update(ctx, current); err != nil {
							t.Fatal(err)
						}
					}}
				}
				changed, err := r.labelNodes(ctx, group, []v1alpha1.ProvisionedNodeClaim{*child})
				want := scenario == "linux" || scenario == "windows"
				wantErr := scenario == "another-group" || scenario == "node-race" || scenario == "machine-race"
				if changed != want || (err != nil) != wantErr {
					t.Fatalf("changed=%v err=%v", changed, err)
				}
				current := &corev1.Node{}
				if err := workload.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
					t.Fatal(err)
				}
				label := ""
				if want {
					label = string(group.UID)
				}
				if scenario == "another-group" {
					label = "other-group"
				}
				if scenario == "node-race" {
					label = "concurrent-owner"
				}
				if current.Labels[GroupUIDLabel] != label || current.Labels["unrelated"] != "keep" {
					t.Fatal("Node ownership or unrelated labels changed", current.Labels)
				}
				if want {
					version := current.ResourceVersion
					if changed, err := r.labelNodes(ctx, group, []v1alpha1.ProvisionedNodeClaim{*child}); err != nil || changed {
						t.Fatal("repeat was not idempotent", changed, err)
					}
					if err := workload.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil || current.ResourceVersion != version {
						t.Fatal("idempotent label rewrote Node", err)
					}
				}
			})
		}
	}
}

type labelRaceClient struct {
	client.Client
	mutate func()
}

func (c *labelRaceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.mutate()
	return c.Client.Patch(ctx, obj, patch, opts...)
}
