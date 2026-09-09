package nodegroup

import (
	"context"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Model the native capacity run: CAPI Ready and NodeReady alone precede the
// worker's adoption of the current mesh document. OS and provider are orthogonal
// to this readiness contract; management and workload reads may be separate.
func TestReadyChildrenRequireAssociatedNodeAndCurrentPeerReceipt(t *testing.T) {
	for _, os := range []string{"linux", "windows"} {
		for _, version := range []string{"v1beta1", "v1beta2"} {
			for _, scenario := range []string{"ready", "machine-pending", "stale-generation", "missing-node-ref", "wrong-claim", "wrong-provider", "wrong-node-uid", "node-not-ready", "cordoned", "draining", "marked", "missing-peer", "missing-route", "missing-receipt", "old-document", "old-ack", "source-changed", "node-changed-during-read", "receipt-replaced-during-read", "mesh-changed-during-read"} {
				t.Run(os+"/"+version+"/"+scenario, func(t *testing.T) {
					group := groupFixture()
					child, err := BuildChild(group, 0)
					if err != nil {
						t.Fatal(err)
					}
					child.UID = "child"
					gvk := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: version, Kind: "Machine"}
					condition := map[string]interface{}{"type": "Ready", "status": "True"}
					if version == "v1beta2" {
						condition["observedGeneration"] = int64(2)
					}
					machine := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"providerID": "provider:///worker"}, "status": map[string]interface{}{"conditions": []interface{}{condition}, "nodeRef": map[string]interface{}{"name": "worker", "uid": "node"}}}}
					machine.SetGroupVersionKind(gvk)
					machine.SetName(child.Name)
					machine.SetNamespace(child.Namespace)
					machine.SetUID("machine")
					machine.SetGeneration(2)
					machine.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(child, v1alpha1.GroupVersion.WithKind("ProvisionedNodeClaim"))})
					machine.SetAnnotations(map[string]string{"cloud-provisioning.appmana.com/wireguard-addr4": "10.100.0.2/24"})
					node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node", Labels: map[string]string{corev1.LabelOSStable: os}}, Spec: corev1.NodeSpec{ProviderID: "provider:///worker"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
					mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: group.Namespace, UID: "mesh"}, Data: map[string][]byte{
						tunnel.NodePublicKeyPrefix + "site": []byte("site-key"), tunnel.NodeTunnelAddressPrefix + "site": []byte("10.100.0.1"),
						tunnel.PeerPublicKeyPrefix + child.Name: []byte("worker-key"), tunnel.PeerRouteHostsPrefix + child.Name: []byte("10.100.0.2"), tunnel.PeerAllowedIPsPrefix + child.Name: []byte("10.100.0.2/32"), tunnel.APIServersKey: []byte("10.10.0.10"),
					}}
					doc, err := tunnel.RemotePeerDocument(mesh.Data, "10.100.0.2", "10.10.0.10", "6443")
					if err != nil || len(doc) == 0 {
						t.Fatal("fixture document", err)
					}
					secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName(child.Name), Namespace: child.Namespace, UID: "adoption", Annotations: map[string]string{tunnel.AppliedListAnnotation: tunnel.HashPeerList(doc)}}, Data: map[string][]byte{tunnel.CloudPeersKey: doc}}
					switch scenario {
					case "machine-pending":
						condition["status"] = "False"
					case "stale-generation":
						condition["observedGeneration"] = int64(1)
					case "missing-node-ref":
						unstructured.RemoveNestedField(machine.Object, "status", "nodeRef")
					case "wrong-claim":
						machine.SetOwnerReferences(nil)
					case "wrong-provider":
						node.Spec.ProviderID = "other:///worker"
					case "wrong-node-uid":
						node.UID = "replacement-node"
					case "node-not-ready":
						node.Status.Conditions[0].Status = corev1.ConditionFalse
					case "cordoned":
						node.Spec.Unschedulable = true
					case "draining":
						group.Status.PendingAction = &v1alpha1.NodeGroupAction{Type: DrainAction, ChildUID: string(child.UID)}
					case "marked":
						a := machine.GetAnnotations()
						a[attachment.DrainIntentAnnotation] = "group/action"
						machine.SetAnnotations(a)
					case "missing-peer":
						delete(mesh.Data, tunnel.PeerPublicKeyPrefix+child.Name)
					case "missing-route":
						delete(mesh.Data, tunnel.PeerRouteHostsPrefix+child.Name)
					case "old-document":
						secret.Data[tunnel.CloudPeersKey] = []byte(`{"peers":[]}`)
						secret.Annotations[tunnel.AppliedListAnnotation] = tunnel.HashPeerList(secret.Data[tunnel.CloudPeersKey])
					case "old-ack":
						secret.Annotations[tunnel.AppliedListAnnotation] = "old"
					case "source-changed":
						mesh.Data[tunnel.APIServersKey] = []byte("10.10.0.14")
					}
					scheme := runtime.NewScheme()
					_ = corev1.AddToScheme(scheme)
					_ = v1alpha1.AddToScheme(scheme)
					objects := []client.Object{group, child, machine, mesh}
					if scenario != "missing-receipt" {
						objects = append(objects, secret)
					}
					api := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.ProvisionedNodeGroupClaim{}).WithObjects(objects...).Build()
					workload := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.ProvisionedNodeGroupClaim{}).WithObjects(node).Build()
					r := &Reconciler{API: api, Workload: workload, MachineGVK: gvk, MeshName: "mesh", APIVIP: "10.10.0.10", APIPort: "6443"}
					if scenario == "node-changed-during-read" || scenario == "receipt-replaced-during-read" || scenario == "mesh-changed-during-read" {
						r.API = &readinessRaceClient{Client: api, key: client.ObjectKeyFromObject(secret), mutate: func() {
							ctx := context.Background()
							switch scenario {
							case "node-changed-during-read":
								n := &corev1.Node{}
								if err := workload.Get(ctx, client.ObjectKeyFromObject(node), n); err != nil {
									t.Fatal(err)
								}
								n.Spec.ProviderID = "replacement:///worker"
								if err := workload.Update(ctx, n); err != nil {
									t.Fatal(err)
								}
							case "receipt-replaced-during-read":
								if err := api.Delete(ctx, secret); err != nil {
									t.Fatal(err)
								}
								replacement := secret.DeepCopy()
								replacement.UID = "replacement-adoption"
								replacement.ResourceVersion = ""
								if err := api.Create(ctx, replacement); err != nil {
									t.Fatal(err)
								}
							case "mesh-changed-during-read":
								m := &corev1.Secret{}
								if err := api.Get(ctx, client.ObjectKeyFromObject(mesh), m); err != nil {
									t.Fatal(err)
								}
								m.Data[tunnel.APIServersKey] = []byte("10.10.0.14")
								if err := api.Update(ctx, m); err != nil {
									t.Fatal(err)
								}
							}
						}}
					}

					count, err := r.readyChildren(context.Background(), group, []v1alpha1.ProvisionedNodeClaim{*child})
					want := int32(0)
					if scenario == "ready" {
						want = 1
					}
					if err != nil || count != want {
						t.Fatalf("Ready count=%d want=%d err=%v", count, want, err)
					}
					if scenario == "missing-node-ref" {
						current := &v1alpha1.ProvisionedNodeGroupClaim{}
						if err := api.Get(context.Background(), client.ObjectKeyFromObject(group), current); err != nil {
							t.Fatal(err)
						}
						current.Finalizers = []string{GroupFinalizer}
						if err := api.Update(context.Background(), current); err != nil {
							t.Fatal(err)
						}
						current.Status.Replicas = 1
						current.Status.PendingAction = &v1alpha1.NodeGroupAction{Type: DrainAction, ID: "pending", ChildUID: string(child.UID), ChildName: child.Name, Ordinal: 0}
						if err := api.Status().Update(context.Background(), current); err != nil {
							t.Fatal(err)
						}
						result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)})
						if err != nil || result.RequeueAfter != 5*time.Second {
							t.Fatalf("unresolved worker did not poll regularly: %v %v", result, err)
						}
					}

				})
			}
		}
	}
}

// Pause the observer after its read, then apply a competing API change before
// its final identity checks. No goroutine timing or cached objects are involved.
type readinessRaceClient struct {
	client.Client
	key    client.ObjectKey
	mutate func()
	fired  bool
}

func (c *readinessRaceClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if err == nil && key == c.key && !c.fired {
		c.fired = true
		c.mutate()
	}
	return err
}
