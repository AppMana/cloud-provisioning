package runtime

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type stepProbe struct {
	calls   int
	id      string
	desired *attachment.GatewayRequest
	phase   attachment.Phase
}

func (p *stepProbe) Step(_ context.Context, id string, d *attachment.GatewayRequest) (attachment.Phase, error) {
	p.calls++
	p.id = id
	p.desired = d
	return p.phase, nil
}
func requestFixture(t *testing.T) (*RequestController, *stepProbe, ctrl.Request) {
	t.Helper()
	machine := func(uid, ip string) attachment.Machine {
		return attachment.Machine{UID: uid, NodeUID: "node-" + uid, ProviderID: "provider-" + uid, InterfaceID: "eni-" + uid, NetworkID: "network", Subnet: netip.MustParsePrefix("172.29.0.0/25"), Address: netip.MustParseAddr(ip)}
	}
	desired := attachment.GatewayRequest{Worker: machine("worker", "172.29.0.21"), Gateway: machine("gateway", "172.29.0.9"), SiteTransport: []netip.Addr{netip.MustParseAddr("10.10.0.11")}, Underlay: []netip.Addr{netip.MustParseAddr("198.51.100.1")}, UDPPort: 4789}
	raw, _ := json.Marshal(desired)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "test", UID: "first", Labels: map[string]string{RequestLabel: "mesh"}}, Data: map[string]string{"request.json": string(raw)}}
	scheme := k8sruntime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	probe := &stepProbe{phase: attachment.Preparing}
	r := &RequestController{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build(), Namespace: "test", MeshName: "mesh", Lifecycle: probe}
	return r, probe, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "test", Name: "worker"}}
}
func TestRequestFinalizesBeforePreparationAndRetainsWithdrawal(t *testing.T) {
	r, p, req := requestFixture(t)
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 {
		t.Fatal("prepared before finalizer")
	}
	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, req.NamespacedName, cm); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cm.Finalizers, requestFinalizer) {
		t.Fatal("request not retained")
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if p.id != "test/worker/first" || p.desired == nil {
		t.Fatal("request identity not bound")
	}
	delete(cm.Labels, RequestLabel)
	if err := r.Client.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	p.phase = attachment.Withdrawing
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if p.desired != nil {
		t.Fatal("label removal did not withdraw")
	}
	if err := r.Client.Get(ctx, req.NamespacedName, cm); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cm.Finalizers, requestFinalizer) {
		t.Fatal("released before withdrawal")
	}
	p.phase = attachment.Complete
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Get(ctx, req.NamespacedName, cm); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cm.Finalizers, requestFinalizer) {
		t.Fatal("completed finalizer retained")
	}
}
func TestDeletingRequestIgnoresInvalidDesired(t *testing.T) {
	r, p, req := requestFixture(t)
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, req.NamespacedName, cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["request.json"] = "invalid"
	if err := r.Client.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}
	p.phase = attachment.Withdrawing
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if p.desired != nil || p.calls != 1 {
		t.Fatal("deletion failed to advance cleanup")
	}
	p.phase = attachment.Complete
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Get(ctx, req.NamespacedName, cm); client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatal("completed request still exists")
	}
}
func TestRequestRejectsInvalidBeforeFinalization(t *testing.T) {
	r, p, req := requestFixture(t)
	ctx := context.Background()
	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, req.NamespacedName, cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["request.json"] = "{}"
	if err := r.Client.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("invalid request accepted")
	}
	if p.calls != 0 {
		t.Fatal("invalid request reached provider")
	}
}

func TestOtherMeshCannotWithdrawOwnedRequest(t *testing.T) {
	r, p, req := requestFixture(t)
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	r.MeshName = "other-mesh"
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 {
		t.Fatal("other mesh advanced owned request")
	}
}
