package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type requestSource struct {
	calls    [][2]string
	err      error
	transfer string
}

func (s *requestSource) Request(_ context.Context, worker, gateway string) (attachment.GatewayRequest, error) {
	s.calls = append(s.calls, [2]string{worker, gateway})
	if s.err != nil {
		return attachment.GatewayRequest{}, s.err
	}
	machine := func(uid, ip string) attachment.Machine {
		return attachment.Machine{UID: uid, NodeUID: "node-" + uid, ProviderID: "provider-" + uid, InterfaceID: "eni-" + uid, NetworkID: "network", Subnet: netip.MustParsePrefix("172.29.0.0/25"), Address: netip.MustParseAddr(ip)}
	}
	return attachment.GatewayRequest{Worker: machine(worker, "172.29.0.21"), Gateway: machine(gateway, "172.29.0.9"),
		SiteTransport: []netip.Addr{netip.MustParseAddr(s.transfer)}, Underlay: []netip.Addr{netip.MustParseAddr("198.51.100.1")}, UDPPort: 4789}, nil
}

func gatewayClaimsFixture(t *testing.T, objects ...client.Object) (*GatewayClaims, *requestSource) {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	source := &requestSource{transfer: "10.10.0.11"}
	return &GatewayClaims{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&v1alpha1.ProvisionedNodeClaim{}).Build(),
		Namespace: "test", MeshName: "mesh", Source: source}, source
}

func claim(name, gateway, machine string) *v1alpha1.ProvisionedNodeClaim {
	return &v1alpha1.ProvisionedNodeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test", UID: types.UID(name + "-uid")},
		Spec: v1alpha1.ProvisionedNodeClaimSpec{Gateway: gateway}, Status: v1alpha1.ProvisionedNodeClaimStatus{MachineName: machine}}
}

func request(t *testing.T, c client.Client, name string) (*corev1.ConfigMap, attachment.GatewayRequest) {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "test", Name: RequestName(name)}, cm); err != nil {
		t.Fatal(err)
	}
	var r attachment.GatewayRequest
	if err := json.Unmarshal([]byte(cm.Data["request.json"]), &r); err != nil {
		t.Fatal(err)
	}
	return cm, r
}

// A claim that names a gateway gets the request the controller derives for
// its Machine and the gateway's, owned by the claim and selected by the
// mesh's attachment controller.
func TestAClaimNamingAGatewayGetsADerivedRequest(t *testing.T) {
	ctx := context.Background()
	g, source := gatewayClaimsFixture(t, claim("win", "gw", "win-machine"), claim("gw", "", "gw-machine"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "test", Name: "win"}}
	if _, err := g.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(source.calls) != 1 || source.calls[0] != [2]string{"win-machine", "gw-machine"} {
		t.Fatalf("derived for %v", source.calls)
	}
	cm, r := request(t, g.Client, "win")
	if cm.Labels[RequestLabel] != "mesh" || len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != "win-uid" || r.Worker.UID != "win-machine" {
		t.Fatalf("request %+v %+v", cm.ObjectMeta, r)
	}

	// A change in what the cluster says becomes a change in the request.
	source.transfer = "10.10.0.12"
	if _, err := g.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, r = request(t, g.Client, "win"); r.SiteTransport[0].String() != "10.10.0.12" {
		t.Fatalf("not rederived: %v", r.SiteTransport)
	}

	// What cannot be derived leaves the standing request alone: a Machine
	// being replaced must not withdraw the attachment its lifetime holds.
	source.err = fmt.Errorf("Machine win-machine is not a running node")
	if _, err := g.Reconcile(ctx, req); err == nil {
		t.Fatal("derivation failure hidden")
	}
	if _, r = request(t, g.Client, "win"); r.SiteTransport[0].String() != "10.10.0.12" {
		t.Fatal("request changed on a failed derivation")
	}
	source.err = nil

	// Clearing the gateway withdraws the request.
	c := &v1alpha1.ProvisionedNodeClaim{}
	if err := g.Client.Get(ctx, req.NamespacedName, c); err != nil {
		t.Fatal(err)
	}
	c.Spec.Gateway = ""
	if err := g.Client.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := g.Client.Get(ctx, client.ObjectKey{Namespace: "test", Name: RequestName("win")}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("request kept: %v", err)
	}
}

// Nothing is derived before both Machines exist, and a request the
// controller did not write is never touched.
func TestGatewayRequestsWaitForMachinesAndLeaveOthersAlone(t *testing.T) {
	ctx := context.Background()
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: RequestName("manual"), Namespace: "test", Labels: map[string]string{RequestLabel: "mesh"}}}
	g, source := gatewayClaimsFixture(t, claim("win", "gw", "win-machine"), claim("gw", "", ""), claim("manual", "", "manual"), foreign)
	if _, err := g.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "test", Name: "win"}}); err != nil {
		t.Fatal(err)
	}
	if len(source.calls) != 0 {
		t.Fatal("derived before the gateway's Machine existed")
	}
	if _, err := g.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "test", Name: "manual"}}); err != nil {
		t.Fatal(err)
	}
	if err := g.Client.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.ConfigMap{}); err != nil {
		t.Fatalf("a request this controller did not write was removed: %v", err)
	}
}
