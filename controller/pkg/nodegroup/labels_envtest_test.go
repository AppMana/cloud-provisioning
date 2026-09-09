package nodegroup

import (
	"context"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func verifyRealAPINodeLabels(t *testing.T, api client.Client) {
	t.Helper()
	ctx := context.Background()
	for _, race := range []string{"update", "replacement"} {
		group := groupFixture()
		group.Name = "node-label-" + race
		group.Namespace = "default"
		group.UID = ""
		if err := api.Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		child, err := BuildChild(group, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := api.Create(ctx, child); err != nil {
			t.Fatal(err)
		}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: group.Name, Labels: map[string]string{"unrelated": "keep"}}, Spec: corev1.NodeSpec{ProviderID: "test:///" + group.Name}}
		if err := api.Create(ctx, node); err != nil {
			t.Fatal(err)
		}
		gvk := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}
		machine := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"providerID": node.Spec.ProviderID}, "status": map[string]interface{}{"nodeRef": map[string]interface{}{"name": node.Name, "uid": string(node.UID)}}}}
		machine.SetGroupVersionKind(gvk)
		machine.SetName(child.Name)
		machine.SetNamespace(child.Namespace)
		machine.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(child, v1alpha1.GroupVersion.WithKind("ProvisionedNodeClaim"))})
		if err := api.Create(ctx, machine); err != nil {
			t.Fatal(err)
		}
		workload := &labelRaceClient{Client: api, mutate: func() {
			current := &corev1.Node{}
			if err := api.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
				t.Fatal(err)
			}
			if race == "replacement" {
				if err := api.Delete(ctx, current); err != nil {
					t.Fatal(err)
				}
				current.ObjectMeta = metav1.ObjectMeta{Name: node.Name, Labels: map[string]string{"unrelated": "replacement"}}
				if err := api.Create(ctx, current); err != nil {
					t.Fatal(err)
				}
				if current.UID == node.UID {
					t.Fatal("fixture did not replace Node UID")
				}
			} else {
				current.Labels["concurrent"] = "keep"
				if err := api.Update(ctx, current); err != nil {
					t.Fatal(err)
				}
			}
		}}
		r := &Reconciler{API: api, Workload: workload, MachineGVK: gvk}
		if changed, err := r.labelNodes(ctx, group, []v1alpha1.ProvisionedNodeClaim{*child}); err == nil || changed {
			t.Fatal("stale label patch passed real API", changed, err)
		}
		current := &corev1.Node{}
		if err := api.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
			t.Fatal(err)
		}
		if current.Labels[GroupUIDLabel] != "" {
			t.Fatal("stale patch labeled current Node")
		}
		r.Workload = api
		changed, err := r.labelNodes(ctx, group, []v1alpha1.ProvisionedNodeClaim{*child})
		if err != nil || changed != (race == "update") {
			t.Fatal("retry adopted a replacement or lost its valid Node", changed, err)
		}
		if race == "update" {
			if err := api.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
				t.Fatal(err)
			}
			if current.Labels[GroupUIDLabel] != string(group.UID) || current.Labels["concurrent"] != "keep" || current.Labels["unrelated"] != "keep" {
				t.Fatal("retry lost labels", current.Labels)
			}
			version := current.ResourceVersion
			if changed, err := r.labelNodes(ctx, group, []v1alpha1.ProvisionedNodeClaim{*child}); err != nil || changed {
				t.Fatal("repeated API labeling changed Node", changed, err)
			}
			if err := api.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil || current.ResourceVersion != version {
				t.Fatal("idempotent patch changed Node version", err)
			}
		}
	}
}
