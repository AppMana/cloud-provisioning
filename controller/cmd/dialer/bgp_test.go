package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The nodes that need to be told about a remote are the ones that
// cannot reach it, and those are exactly the nodes with no tunnel. A
// node with no tunnel has no entry in the mesh, so taking the audience
// from the peer Secret yields nobody: every entry there is a machine
// somewhere else. A neighbour that was never configured has its
// connection reset, so this is the difference between transit working
// and the site's routers reporting a peer they cannot establish.
func TestReconcileTransit_TellsTheNodesThatAreNotInTheMesh(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "peers", Namespace: "cloud-provisioning"},
			Data: map[string][]byte{
				"peer-public-key-cloud-1":  []byte("cloudpubkey1"),
				"peer-endpoint-cloud-1":    []byte("203.0.113.10:51820"),
				"peer-allowed-ips-cloud-1": []byte("10.100.0.128/32,10.244.123.128/26"),
				"peer-route-hosts-cloud-1": []byte("10.100.0.128"),
			},
		},
		node("control-plane", "172.21.0.18"),
		node("endpoint", "172.21.0.17"),
		node("no-tunnel", "172.21.0.19"),
		// The remote is a node too. It is reached through the tunnel,
		// not told about it.
		node("cloud-1", "10.100.0.128"),
	)

	speaker, err := startTransitSpeaker(ctx, 17900, 64512, "172.21.0.17")
	if err != nil {
		t.Fatalf("startTransitSpeaker: %v", err)
	}
	defer speaker.stop(ctx)
	cfg := config{secretName: "peers", secretNamespace: "cloud-provisioning"}
	if err := reconcileTransit(ctx, clientset, cfg, speaker); err != nil {
		t.Fatalf("reconcileTransit: %v", err)
	}

	for _, want := range []string{"172.21.0.18", "172.21.0.19"} {
		if !speaker.peers[want] {
			t.Errorf("node %s was never peered with, so it cannot learn the transit route", want)
		}
	}
	// Peering with the remote would be peering across the tunnel with a
	// node that already has the route.
	if speaker.peers["10.100.0.128"] {
		t.Error("the remote was peered with; only this site's nodes should be")
	}
	// This node is its own next hop; a session with itself is not one.
	if speaker.peers["172.21.0.17"] {
		t.Error("the speaker peered with itself")
	}
	if !speaker.advertised["10.100.0.128/32"] {
		t.Error("the remote node address was not advertised")
	}
	// The block has to be carried too. The site learns the node address
	// over BGP, and a BGP next hop is not resolved by another BGP route,
	// so the block the remote advertises directly stays unreachable.
	if !speaker.advertised["10.244.123.128/26"] {
		t.Error("the remote pod block was not advertised, so the site cannot resolve it")
	}
}

func node(name, addr string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: addr}},
		},
	}
}

// A speaker with no port is the ordinary case: a site whose CNI has no
// router to tell, or a node that terminates no tunnel. Every call has to
// be safe on it.
func TestTransitSpeaker_DisabledIsInert(t *testing.T) {
	s, err := startTransitSpeaker(context.Background(), 0, 64512, "10.0.0.1")
	if err != nil {
		t.Fatalf("startTransitSpeaker: %v", err)
	}
	if s != nil {
		t.Fatal("a zero port should not start a speaker")
	}
	if err := s.reconcile(context.Background(), []string{"10.0.0.2"}, []transitRoute{{prefix: "10.1.0.1/32"}}); err != nil {
		t.Errorf("reconcile on a disabled speaker: %v", err)
	}
	s.stop(context.Background())
}

// The next hop is what the site routes to, so a speaker without one
// would advertise routes nobody can use.
func TestTransitSpeaker_RefusesWithoutANextHop(t *testing.T) {
	if _, err := startTransitSpeaker(context.Background(), 1790, 64512, ""); err == nil {
		t.Fatal("expected an error without a next hop, got nil")
	}
}

// Advertising anything but an address would put a prefix into the site's
// routing that no node owns.
func TestTransitSpeaker_RefusesANonAddress(t *testing.T) {
	if _, err := hostRoute("10.244.0.0/16", 0); err == nil {
		t.Fatal("expected an error for a prefix where an address is required, got nil")
	}
	if _, err := hostRoute("", 0); err == nil {
		t.Fatal("expected an error for an empty value, got nil")
	}
	// A block is a prefix, but never one that would take everything.
	if _, err := blockRoute("0.0.0.0/0", 0); err == nil {
		t.Fatal("expected an error for a default route, got nil")
	}
	if got, err := blockRoute("10.244.123.128/26", 3); err != nil || got.prefix != "10.244.123.128/26" || got.med != 3 {
		t.Fatalf("blockRoute = %+v, %v; want the masked prefix carrying its preference", got, err)
	}
}
