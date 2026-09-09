package nodegroup

import (
	"context"
	"encoding/json"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func verifyPeerWithdrawalCapture(t *testing.T, api client.Client) {
	t.Helper()
	ctx := context.Background()
	g := groupFixture()
	g.Name = "peer-withdrawal"
	g.Namespace = "default"
	g.UID = ""
	zero := int32(0)
	g.Spec.Replicas = &zero
	if err := api.Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	child, err := BuildChild(g, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Create(ctx, child); err != nil {
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
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "withdrawal-mesh", Namespace: g.Namespace}, Data: map[string][]byte{"private-key": []byte("must-stay-in-secret")}}
	for _, name := range []string{child.Name, "withdrawal-survivor"} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{ProviderID: "test://" + name}}
		if err := api.Create(ctx, node); err != nil {
			t.Fatal(err)
		}
		machine := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cluster.x-k8s.io/v1beta2", "kind": "Machine", "metadata": map[string]any{"name": name, "namespace": g.Namespace, "annotations": map[string]any{"cloud-provisioning.appmana.com/wireguard-addr4": "10.100.0.2/24"}}, "spec": map[string]any{"providerID": node.Spec.ProviderID}, "status": map[string]any{"nodeRef": map[string]any{"name": name}}}}
		if name == child.Name {
			a := machine.GetAnnotations()
			a[attachment.DrainIntentAnnotation] = string(g.UID) + "/" + g.Status.PendingAction.ID
			machine.SetAnnotations(a)
		}
		if err := api.Create(ctx, machine); err != nil {
			t.Fatal(err)
		}
		adoption := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName(name), Namespace: g.Namespace}}
		if err := api.Create(ctx, adoption); err != nil {
			t.Fatal(err)
		}
		mesh.Data[tunnel.PeerPublicKeyPrefix+name] = []byte("key-" + name)
		mesh.Data[tunnel.PeerRouteHostsPrefix+name] = []byte("10.100.0.2")
		mesh.Data[tunnel.PeerAllowedIPsPrefix+name] = []byte("10.100.0.2/32")
		if name == child.Name {
			a := g.Status.PendingAction
			a.NodeName = node.Name
			a.NodeUID = string(node.UID)
			a.MachineUID = string(machine.GetUID())
			a.ProviderID = node.Spec.ProviderID
		}
	}
	site := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "zz-withdrawal-site"}}
	if err := api.Create(ctx, site); err != nil {
		t.Fatal(err)
	}
	mesh.Data[tunnel.NodePublicKeyPrefix+site.Name] = []byte("site-key")
	mesh.Data[tunnel.NodeTunnelAddressPrefix+site.Name] = []byte("10.100.0.1")
	mesh.Data[tunnel.NodeAddressesPrefix+site.Name] = []byte("10.10.0.11")
	if err := api.Create(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	g.Status.PendingAction.Gateways = &v1alpha1.GroupGatewayInventory{Mesh: mesh.Name, Requests: []v1alpha1.GroupGatewayRequest{}}
	if err := api.Status().Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	captured, err := CapturePeerWithdrawal(ctx, api, g)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status.PendingAction.Withdrawal != nil {
		t.Fatal("capture mutated caller")
	}
	reloaded := &v1alpha1.ProvisionedNodeGroupClaim{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(g), reloaded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(captured.Status.PendingAction.Withdrawal, reloaded.Status.PendingAction.Withdrawal) {
		t.Fatal("API pruned public inventory")
	}
	w := reloaded.Status.PendingAction.Withdrawal
	if w.MeshUID != string(mesh.UID) || w.SourceVersion != mesh.ResourceVersion || len(w.Consumers) != 2 || w.Consumers[0].MachineName != "withdrawal-survivor" || w.Consumers[0].SecretUID == "" {
		t.Fatalf("lost identities: %+v", w)
	}
	copied := reloaded.DeepCopy()
	copied.Status.PendingAction.Withdrawal.Consumers[0].NodeUID = "replacement"
	if reflect.DeepEqual(copied.Status.PendingAction.Withdrawal, w) {
		t.Fatal("deep copy shares recipients")
	}
	if _, err := CapturePeerWithdrawal(ctx, api, g); !apierrors.IsConflict(err) {
		t.Fatalf("stale capture accepted: %v", err)
	}
	if _, err := CapturePeerWithdrawal(ctx, api, reloaded); err == nil {
		t.Fatal("captured inventory overwritten")
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(mesh), mesh); err != nil {
		t.Fatal(err)
	}
	if len(mesh.Data[tunnel.PeerPublicKeyPrefix+child.Name]) == 0 {
		t.Fatal("capture prematurely removed peer")
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil {
		t.Fatal("capture deleted child", err)
	}
	gvk := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}
	mesh.Data[tunnel.PeerPublicKeyPrefix+"late-join"] = []byte("late-key")
	if err := api.Update(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	if applied, err := ApplyPeerWithdrawal(ctx, api, gvk, reloaded, "", "6443"); err == nil || applied {
		t.Fatal("new recipient bypassed retained inventory", applied, err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(mesh), mesh); err != nil {
		t.Fatal(err)
	}
	if len(mesh.Data[tunnel.PeerPublicKeyPrefix+child.Name]) == 0 {
		t.Fatal("membership error still removed peer")
	}
	delete(mesh.Data, tunnel.PeerPublicKeyPrefix+"late-join")
	if err := api.Update(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	if applied, err := ApplyPeerWithdrawal(ctx, api, gvk, reloaded, "", "6443"); err != nil || applied {
		t.Fatal("withdrawal acknowledged without receipts", applied, err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(mesh), mesh); err != nil {
		t.Fatal(err)
	}
	if len(mesh.Data[tunnel.PeerPublicKeyPrefix+child.Name]) != 0 {
		t.Fatal("peer not withdrawn")
	}
	// Real Secret writes model independently arriving native acknowledgements.
	// This verifies controller gating, not a native WireGuard application.
	hash, err := tunnel.SitePeerHash(mesh.Data)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := json.Marshal(tunnel.SiteApplied{NodeUID: string(site.UID), PublicKey: "site-key", Hash: hash})
	if err != nil {
		t.Fatal(err)
	}
	mesh.Data[tunnel.SiteAppliedPrefix+site.Name] = ack
	if err := api.Update(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	if applied, err := ApplyPeerWithdrawal(ctx, api, gvk, reloaded, "", "6443"); err != nil || applied {
		t.Fatal("site receipt bypassed remote receipt", applied, err)
	}
	doc, err := tunnel.RemotePeerDocument(mesh.Data, "10.100.0.2", "", "6443")
	if err != nil || len(doc) == 0 {
		t.Fatal("missing survivor document", err)
	}
	secret := &corev1.Secret{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: g.Namespace, Name: tunnel.AdoptionSecretName("withdrawal-survivor")}, secret); err != nil {
		t.Fatal(err)
	}
	secret.Data = map[string][]byte{tunnel.CloudPeersKey: doc}
	secret.Annotations = map[string]string{tunnel.AppliedListAnnotation: tunnel.HashPeerList(doc)}
	if err := api.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(g), reloaded); err != nil {
		t.Fatal(err)
	}
	if applied, err := ApplyPeerWithdrawal(ctx, api, gvk, reloaded, "", "6443"); err != nil || !applied {
		t.Fatal("retained recipients did not converge", applied, err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil {
		t.Fatal("publication deleted child", err)
	}
	if err := api.Delete(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if applied, err := ApplyPeerWithdrawal(ctx, api, gvk, reloaded, "", "6443"); err != nil || applied {
		t.Fatal("missing recipient acknowledged withdrawal", applied, err)
	}

}
