package join

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// On a dual-stack mesh a remote's identity carries an IPv6 tunnel
// address paired with its IPv4 allocation, the site permits and routes
// it on the remote's entry, and the Machine records it so the CNI can be
// told to peer on it.
func TestReconcile_DualStackRemoteGetsAnIPv6TunnelAddress(t *testing.T) {
	machine := machineWithInfraRef("cloud-worker-0", "default", "cloud-worker-0")
	awsMachine := fakeAWSMachine("cloud-worker-0", "default", true)
	join := &stubJoinProvider{values: map[string]any{"joinToken": "fake-token", "k0sVersion": "v1.36.4+k0s.1"}}
	r := newFakeJoinReconciler(t, join, machine, awsMachine, dialerPeerSecretFixture())
	r.WireGuardAddress6Prefix = "fd00:10:100::/96"

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(machine), updated); err != nil {
		t.Fatal(err)
	}
	if got := updated.GetAnnotations()[WireGuardAddr6Annotation]; got != "fd00:10:100::a64:80/96" {
		t.Errorf("%s = %q, want fd00:10:100::a64:80/96", WireGuardAddr6Annotation, got)
	}
	mesh := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "wg-dialer", Name: "wg-dialer-peer"}, mesh); err != nil {
		t.Fatal(err)
	}
	if got, want := string(mesh.Data[tunnel.PeerAllowedIPsPrefix+"cloud-worker-0"]), "10.100.0.128/32,fd00:10:100::a64:80/128"; got != want {
		t.Errorf("peer-allowed-ips = %q, want %q", got, want)
	}
	if got, want := string(mesh.Data[tunnel.PeerRouteHostsPrefix+"cloud-worker-0"]), "10.100.0.128,fd00:10:100::a64:80"; got != want {
		t.Errorf("peer-route-hosts = %q, want %q", got, want)
	}
	bootstrap := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cloud-worker-0-bootstrap"}, bootstrap); err != nil {
		t.Fatal(err)
	}
	rendered := secretValue(bootstrap, "value")
	raw := rendered[strings.Index(rendered, "peersFileJSON=")+len("peersFileJSON="):]
	var doc tunnel.PeersFileDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("baked peers file: %v (%s)", err, raw)
	}
	if doc.LocalAddress != "10.100.0.128/24" || doc.LocalAddress6 != "fd00:10:100::a64:80/96" {
		t.Errorf("baked identity addresses = %q, %q", doc.LocalAddress, doc.LocalAddress6)
	}
}

// A single-stack mesh renders exactly what it did before.
func TestReconcile_SingleStackRemoteHasNoIPv6TunnelAddress(t *testing.T) {
	machine := machineWithInfraRef("cloud-worker-0", "default", "cloud-worker-0")
	awsMachine := fakeAWSMachine("cloud-worker-0", "default", true)
	join := &stubJoinProvider{values: map[string]any{"joinToken": "fake-token", "k0sVersion": "v1.36.4+k0s.1"}}
	r := newFakeJoinReconciler(t, join, machine, awsMachine, dialerPeerSecretFixture())
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(machine), updated); err != nil {
		t.Fatal(err)
	}
	if got, ok := updated.GetAnnotations()[WireGuardAddr6Annotation]; ok {
		t.Errorf("single-stack Machine carries %s=%q", WireGuardAddr6Annotation, got)
	}
}
