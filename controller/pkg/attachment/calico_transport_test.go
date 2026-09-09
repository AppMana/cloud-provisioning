package attachment

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"net/netip"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

type nativeObserved bool

func (n nativeObserved) Ready(context.Context, Machine, string) (bool, error) { return bool(n), nil }
func TestCalicoTransportAtomicOriginalAndReplacementProtection(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node", Annotations: map[string]string{calicoAddress: "10.100.0.2/32", "unrelated": "preserved"}}, Spec: corev1.NodeSpec{ProviderID: "aws:///zone/i-worker"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	record := Record{ID: "worker", Lease: "epoch", Digest: "digest", Plan: GatewayPlan{Worker: Machine{NodeUID: "node", ProviderID: node.Spec.ProviderID, Address: netip.MustParseAddr("172.29.0.21"), Subnet: netip.MustParsePrefix("172.29.0.0/25")}}}
	transport := CalicoTransport{Client: c, Observer: nativeObserved(false)}
	if ok, err := transport.Ensure(ctx, record); err != nil || ok {
		t.Fatalf("annotation alone declared CNI ready %v %v", ok, err)
	}
	current := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: "worker"}, current); err != nil {
		t.Fatal(err)
	}
	if owned, err := NativeTransportOwned(current); err != nil || !owned || current.Annotations[calicoAddress] != "172.29.0.21/25" {
		t.Fatal("original journal not committed with address")
	}
	transport.Observer = nativeObserved(true)
	if ok, err := transport.Ensure(ctx, record); err != nil || !ok {
		t.Fatal(err)
	}
	other := record
	other.Lease = "other"
	if _, err := transport.Restore(ctx, other); err == nil {
		t.Fatal("other lease restored address")
	}
	if ok, err := transport.Restore(ctx, record); err != nil || !ok {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "worker"}, current); err != nil {
		t.Fatal(err)
	}
	if current.Annotations[calicoAddress] != "10.100.0.2/32" || current.Annotations[NativeTransportOwner] != "" || current.Annotations["unrelated"] != "preserved" {
		t.Fatal("original state not restored")
	}
	if _, err := transport.Ensure(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	replacement := node.DeepCopy()
	replacement.UID = "replacement"
	replacement.ResourceVersion = ""
	replacement.Annotations[calicoAddress] = "172.29.0.99/25"
	if err := c.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if ok, err := transport.Restore(ctx, record); err != nil || !ok {
		t.Fatal("deleted target could not retire")
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "worker"}, current); err != nil || current.Annotations[calicoAddress] != "172.29.0.99/25" {
		t.Fatal("changed replacement Node")
	}
}
