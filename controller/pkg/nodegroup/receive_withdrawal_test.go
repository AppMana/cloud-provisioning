package nodegroup

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Discovery boundary only: fake APIs verify that reserved/malformed state cannot
// fall through to ordinary peer removal. Full drain-to-receipt execution, real
// API concurrency, compute deletion and traffic continuity require integration.
func TestReceiveRemovalDiscovery(t *testing.T) {
	for _, state := range []string{"absent", "empty", "reserved", "missing-api", "malformed", "replaced-index", "wrong-mesh"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			group := groupFixture()
			group.UID = "group"
			group.ResourceVersion = "1"
			group.Status.PendingAction = &v1alpha1.NodeGroupAction{Type: DrainAction, ID: "drain", ChildName: "worker", ChildUID: "child", MachineUID: "machine", NodeName: "worker", NodeUID: "node", ProviderID: "test:///worker", Gateways: &v1alpha1.GroupGatewayInventory{Mesh: "mesh"}, Withdrawal: &v1alpha1.GroupPeerWithdrawal{MeshName: "mesh", MeshUID: "mesh-uid"}}
			mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: group.Namespace, Name: "mesh", UID: "mesh-uid", ResourceVersion: "1"}, Data: map[string][]byte{tunnel.PeerPublicKeyPrefix + "worker": []byte("root")}}
			if state == "wrong-mesh" {
				mesh.UID = "other"
			}
			inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: "cluster", Namespace: group.Namespace, MeshName: "mesh", MeshUID: "mesh-uid", MachineUIDs: []string{}}
			if state == "reserved" {
				inventory.MachineUIDs = []string{"machine"}
			}
			raw, err := json.Marshal(inventory)
			if err != nil {
				t.Fatal(err)
			}
			index := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: group.Namespace, Name: tunnel.ReceiveInventoryName("mesh-uid"), UID: "index", ResourceVersion: "1"}, Data: map[string]string{"inventory.json": string(raw)}}
			if state == "malformed" {
				index.Data["inventory.json"] = "{}"
			}
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = v1alpha1.AddToScheme(scheme)
			objects := []client.Object{group, mesh}
			if state != "absent" {
				objects = append(objects, index)
			}
			management := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(group).WithObjects(objects...).Build()
			coreIndex := index.DeepCopy()
			if state == "replaced-index" {
				coreIndex.UID = "replacement"
			}
			core := kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster"}}, coreIndex)
			r := &Reconciler{API: management, RetirementAPI: core}
			if state == "missing-api" {
				r.RetirementAPI = nil
			}
			handled, err := r.beginReceiveRemoval(ctx, group)
			if state == "absent" || state == "empty" {
				if handled || err != nil {
					t.Fatal("unreserved worker intercepted", handled, err)
				}
			} else if !handled || err == nil {
				t.Fatal("unsafe receiver discovery fell through", handled, err)
			}
			current := &v1alpha1.ProvisionedNodeGroupClaim{}
			if err := management.Get(ctx, client.ObjectKeyFromObject(group), current); err != nil {
				t.Fatal(err)
			}
			if current.Status.PendingAction.Removing {
				t.Fatal("discovery authorized compute removal")
			}
			published := &corev1.Secret{}
			if err := management.Get(ctx, client.ObjectKeyFromObject(mesh), published); err != nil {
				t.Fatal(err)
			}
			if string(published.Data[tunnel.PeerPublicKeyPrefix+"worker"]) != "root" {
				t.Fatal("discovery removed peer")
			}
		})
	}
}
