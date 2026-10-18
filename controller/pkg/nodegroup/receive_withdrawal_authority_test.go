package nodegroup

import (
	"context"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type withdrawalWorkloadReader struct {
	client.Reader
	beforeList func() error
}

func (r withdrawalWorkloadReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := r.beforeList(); err != nil {
		return err
	}
	return r.Reader.List(ctx, list, opts...)
}

// Fake API lifecycle admission only. Verifies fresh identity, captured recipient
// equality and workload drain policy without mutation. No real API race, native
// cleanup, eviction/PDB behavior or compute-deletion qualification is implied.
func TestReceiveWithdrawalDrainAuthority(t *testing.T) {
	for _, version := range []string{"v1beta1", "v1beta2"} {
		for _, fault := range []string{"none", "daemon", "static", "completed", "workload", "terminating-workload", "uncordoned", "node-uid", "provider", "machine-uid", "marker", "child-uid", "group-action", "node-race", "machine-race", "intent", "recipients", "removing"} {
			t.Run(version+"/"+fault, func(t *testing.T) {
				ctx := context.Background()
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				group := groupFixture()
				group.UID = "group-uid"
				group.ResourceVersion = "1"
				group.Generation = 1
				child, err := BuildChild(group, 0)
				must(err)
				child.UID = "child-uid"
				group.Status.PendingAction = &v1alpha1.NodeGroupAction{Type: DrainAction, ID: "drain", ChildName: child.Name, ChildUID: string(child.UID), MachineUID: "machine-uid", NodeName: "worker", NodeUID: "node-uid", ProviderID: "test:///worker", Gateways: &v1alpha1.GroupGatewayInventory{Mesh: "mesh", Requests: []v1alpha1.GroupGatewayRequest{}}, Withdrawal: &v1alpha1.GroupPeerWithdrawal{MeshName: "mesh", MeshUID: "mesh-uid", SourceVersion: "mesh-version", PublicKey: "root", Consumers: []v1alpha1.GroupPeerConsumer{{Site: true, NodeName: "site", NodeUID: "site-uid", PublicKey: "site-key"}}}}
				intent := attachment.RemoteWithdrawalIntent{MeshNamespace: group.Namespace, MeshName: "mesh", MeshUID: "mesh-uid", SourceVersion: "mesh-version", MachineName: child.Name, MachineUID: "machine-uid", PublicKey: "root", DrainIntent: "group-uid/drain", Consumers: []attachment.ConsumerTarget{{Site: true, NodeName: "site", NodeUID: "site-uid", PublicKey: "site-key"}}}
				gvk := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: version, Kind: "Machine"}
				machine := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"providerID": "test:///worker"}, "status": map[string]interface{}{"nodeRef": map[string]interface{}{"name": "worker", "uid": "node-uid"}}}}
				machine.SetGroupVersionKind(gvk)
				machine.SetName(child.Name)
				machine.SetNamespace(child.Namespace)
				machine.SetUID("machine-uid")
				machine.SetAnnotations(map[string]string{attachment.DrainIntentAnnotation: intent.DrainIntent})
				machine.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(child, v1alpha1.GroupVersion.WithKind("ProvisionedNodeClaim"))})
				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node-uid"}, Spec: corev1.NodeSpec{ProviderID: "test:///worker", Unschedulable: true}}
				liveGroup := group.DeepCopy()
				switch fault {
				case "uncordoned":
					node.Spec.Unschedulable = false
				case "node-uid":
					node.UID = "other"
				case "provider":
					node.Spec.ProviderID = "test:///other"
				case "machine-uid":
					machine.SetUID("other")
				case "marker":
					machine.SetAnnotations(nil)
				case "child-uid":
					child.UID = "other"
				case "group-action":
					liveGroup.Status.PendingAction.ID = "other"
				case "intent":
					intent.DrainIntent = "other"
				case "recipients":
					intent.Consumers = nil
				case "removing":
					group.Status.PendingAction.Removing = true
					liveGroup = group.DeepCopy()
				}
				scheme := runtime.NewScheme()
				must(v1alpha1.AddToScheme(scheme))
				must(corev1.AddToScheme(scheme))
				management := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(group).WithObjects(liveGroup, child, machine).Build()
				objects := []client.Object{node}
				if fault == "daemon" || fault == "static" || fault == "completed" || fault == "workload" || fault == "terminating-workload" {
					pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "probe", UID: "pod-uid"}, Spec: corev1.PodSpec{NodeName: node.Name}}
					switch fault {
					case "daemon":
						controller := true
						pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "agent", UID: "daemon", Controller: &controller}}
					case "static":
						pod.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "mirror"}
					case "completed":
						pod.Status.Phase = corev1.PodSucceeded
					case "terminating-workload":
						now := metav1.Now()
						pod.DeletionTimestamp = &now
						pod.Finalizers = []string{"test/hold"}
					}
					objects = append(objects, pod)
				}
				workload := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string { return []string{obj.(*corev1.Pod).Spec.NodeName} }).Build()
				var reader client.Reader = workload
				if fault == "node-race" || fault == "machine-race" {
					reader = withdrawalWorkloadReader{Reader: workload, beforeList: func() error {
						if fault == "node-race" {
							current := &corev1.Node{}
							if err := workload.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
								return err
							}
							current.Labels = map[string]string{"changed": "true"}
							return workload.Update(ctx, current)
						}
						current := &unstructured.Unstructured{}
						current.SetGroupVersionKind(gvk)
						if err := management.Get(ctx, client.ObjectKeyFromObject(machine), current); err != nil {
							return err
						}
						current.SetLabels(map[string]string{"changed": "true"})
						return management.Update(ctx, current)
					}}
				}
				err = AuthorizeReceiveWithdrawal(ctx, management, reader, gvk, group, intent)
				valid := fault == "none" || fault == "daemon" || fault == "static" || fault == "completed"
				if valid {
					must(err)
				} else if err == nil {
					t.Fatal("changed lifecycle authorized publication")
				}
				current := &corev1.Node{}
				must(workload.Get(ctx, client.ObjectKeyFromObject(node), current))
				if current.Spec.Unschedulable != node.Spec.Unschedulable {
					t.Fatal("authorization changed cordon")
				}
				remaining := &corev1.PodList{}
				must(workload.List(ctx, remaining))
				if len(remaining.Items) != len(objects)-1 {
					t.Fatal("authorization removed workload")
				}
				m := &unstructured.Unstructured{}
				m.SetGroupVersionKind(gvk)
				must(management.Get(ctx, client.ObjectKeyFromObject(machine), m))
				if m.GetAnnotations()[attachment.DrainIntentAnnotation] != machine.GetAnnotations()[attachment.DrainIntentAnnotation] {
					t.Fatal("authorization repaired drain marker")
				}
			})
		}
	}
}
