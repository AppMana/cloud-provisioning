package nodegroup

import (
	"context"
	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strings"
	"testing"
)

func TestDrainTargetPinsMachineAndNode(t *testing.T) {
	for _, version := range []string{"v1beta1", "v1beta2"} {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			group := groupFixture()
			group.ResourceVersion = "1"
			group.Generation = 1
			child, err := BuildChild(group, 0)
			if err != nil {
				t.Fatal(err)
			}
			child.UID = "child-uid"
			zero := int32(0)
			group.Spec.Replicas = &zero
			action, err := ProposeAction(group, []v1alpha1.ProvisionedNodeClaim{*child})
			if err != nil {
				t.Fatal(err)
			}
			group.Status.PendingAction = action
			gvk := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: version, Kind: "Machine"}
			machine := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"providerID": "test:///worker"}, "status": map[string]interface{}{"nodeRef": map[string]interface{}{"name": "worker"}}}}
			machine.SetGroupVersionKind(gvk)
			machine.SetName(child.Name)
			machine.SetNamespace(child.Namespace)
			machine.SetUID("machine-uid")
			machine.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(child, v1alpha1.GroupVersion.WithKind("ProvisionedNodeClaim"))})
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node-uid"}, Spec: corev1.NodeSpec{ProviderID: "test:///worker"}}
			scheme := runtime.NewScheme()
			_ = v1alpha1.AddToScheme(scheme)
			_ = corev1.AddToScheme(scheme)
			management := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(group).WithObjects(group, child, machine).Build()
			workload := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string { return []string{obj.(*corev1.Pod).Spec.NodeName} }).Build()
			bound, err := BindDrainNode(ctx, management, workload, gvk, group)
			if err != nil {
				t.Fatal(err)
			}
			if bound.Status.PendingAction.NodeUID != "node-uid" || bound.Status.PendingAction.MachineUID != "machine-uid" || group.Status.PendingAction.NodeUID != "" {
				t.Fatal("target not independently persisted")
			}
			if _, err := BindDrainNode(ctx, management, workload, gvk, bound); err != nil {
				t.Fatal("restart rejected same target", err)
			}

			r := &Reconciler{API: management, Workload: workload, MachineGVK: gvk}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)}
			// Install the group finalizer, then execute the persisted drain target.
			for i := 0; i < 3; i++ {
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatal(err)
				}
			}
			if err := workload.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
				t.Fatal(err)
			}
			if !node.Spec.Unschedulable {
				t.Fatal("reconciler did not cordon bound node")
			}
			if _, err := r.Reconcile(ctx, req); err == nil || !strings.Contains(err.Error(), "attachment withdrawal") {
				t.Fatal("removal skipped withdrawal gate", err)
			}
			remaining := &v1alpha1.ProvisionedNodeClaim{}
			if err := management.Get(ctx, client.ObjectKeyFromObject(child), remaining); err != nil || !remaining.DeletionTimestamp.IsZero() {
				t.Fatal("deleted claim before withdrawal", err)
			}
			if err := workload.Delete(ctx, node); err != nil {
				t.Fatal(err)
			}
			node.ResourceVersion = ""
			node.UID = "replacement-node"
			if err := workload.Create(ctx, node); err != nil {
				t.Fatal(err)
			}
			if _, err := BindDrainNode(ctx, management, workload, gvk, bound); err == nil {
				t.Fatal("adopted replacement node")
			}
			stored := &v1alpha1.ProvisionedNodeGroupClaim{}
			if err := management.Get(ctx, client.ObjectKeyFromObject(group), stored); err != nil {
				t.Fatal(err)
			}
			if stored.Status.PendingAction.NodeUID != "node-uid" {
				t.Fatal("replaced persisted target")
			}
		})
	}
}
