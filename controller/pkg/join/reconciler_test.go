package join

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func dialerPeerSecretFixture() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wg-dialer-peer", Namespace: "wg-dialer"},
		Data: map[string][]byte{
			"dialer-private-key-jarvis":     []byte("8ALeO136hzNidiHffMf40dM/SKi7iJHBvfy+mplpKVY="),
			"local-address-jarvis":          []byte("10.100.0.1/24"),
			"cluster-vip-jarvis":            []byte("10.101.0.1"),
			"dialer-private-key-spark-2ab3": []byte("6Opkhqk2sKOCnwLzGkjvk+4SxB8fuDH0XreH3RefOUo="),
			"local-address-spark-2ab3":      []byte("10.100.0.3/24"),
			"cluster-vip-spark-2ab3":        []byte("10.101.0.2"),
			"peer-public-key":               []byte("unrelated"), // must NOT be treated as a node peer
			"peer-endpoint":                 []byte("unrelated"),
		},
	}
}

func TestOnPremPeers(t *testing.T) {
	peers, err := onPremPeers(dialerPeerSecretFixture())
	if err != nil {
		t.Fatalf("onPremPeers: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("expected exactly 2 peers (not the peer-public-key/peer-endpoint keys treated as nodes), got %d: %+v", len(peers), peers)
	}
	// jarvis's derived public key, confirmed against the real value
	// used this session (private key above is the real jarvis dialer key).
	const jarvisPub = "WpxHfxQmbN8EZZVHeFVhV5uhLWhw2vbY0Rgb0Wnn5Sg="
	found := false
	for _, p := range peers {
		if p["publicKey"] == jarvisPub {
			found = true
			if p["allowedIPs"] != "10.100.0.1/32, 10.101.0.1/32" {
				t.Errorf("jarvis allowedIPs = %q, want both tunnel address AND cluster VIP", p["allowedIPs"])
			}
		}
	}
	if !found {
		t.Errorf("jarvis's derived public key %s not found in peer list", jarvisPub)
	}
}

func TestOnPremPeers_DualStackClusterVIPsGetCorrectPrefixLengths(t *testing.T) {
	// Every real node in this cluster is genuinely dual-stack (confirmed
	// against live Node.status.addresses: each has both a 10.101.0.x and
	// an fd8f:cf26:522a::x InternalIP), so cluster-vip-<node> must be
	// able to carry both, and each must get the correct single-host
	// prefix length for its family -- /32 for IPv4, /128 for IPv6.
	// Hardcoding /32 across the board either fails to parse against an
	// IPv6 literal or silently matches the wrong host count.
	secret := dialerPeerSecretFixture()
	secret.Data["cluster-vip-jarvis"] = []byte("10.101.0.1,fd8f:cf26:522a::1")

	peers, err := onPremPeers(secret)
	if err != nil {
		t.Fatalf("onPremPeers: %v", err)
	}
	const jarvisPub = "WpxHfxQmbN8EZZVHeFVhV5uhLWhw2vbY0Rgb0Wnn5Sg="
	for _, p := range peers {
		if p["publicKey"] != jarvisPub {
			continue
		}
		want := "10.100.0.1/32, 10.101.0.1/32, fd8f:cf26:522a::1/128"
		if p["allowedIPs"] != want {
			t.Errorf("jarvis allowedIPs = %q, want %q", p["allowedIPs"], want)
		}
		return
	}
	t.Fatal("jarvis not found in peer list")
}

func TestOnPremPeers_MissingClusterVIP(t *testing.T) {
	secret := dialerPeerSecretFixture()
	delete(secret.Data, "cluster-vip-jarvis")
	if _, err := onPremPeers(secret); err == nil {
		t.Fatal("expected an error when cluster-vip-<node> is missing, got nil -- this must fail loudly, not silently omit the BGP-required VIP")
	}
}

func TestOnPremPeers_MissingLocalAddress(t *testing.T) {
	secret := dialerPeerSecretFixture()
	delete(secret.Data, "local-address-jarvis")
	if _, err := onPremPeers(secret); err == nil {
		t.Fatal("expected an error when local-address-<node> is missing, got nil")
	}
}

func machineListGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineList"}
}

func fakeMachine(name string, nodeVIPAnnotation string) *unstructured.Unstructured {
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"})
	m.SetName(name)
	m.SetNamespace("default")
	if nodeVIPAnnotation != "" {
		m.SetAnnotations(map[string]string{NodeVIPAnnotation: nodeVIPAnnotation})
	}
	return m
}

func newFakeReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	// Machine/MachineList are only ever used as unstructured here (no
	// generated Go types for CAPI's Machine in this module) -- register
	// them with the scheme as unstructured so the fake client's List()
	// knows what GVK a MachineList corresponds to.
	scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(machineListGVK(), &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Reconciler{Client: c, Reader: c, NodeVIPStart: 4}
}

func TestAllocateNodeVIPIndex_NoExisting(t *testing.T) {
	r := newFakeReconciler(t)
	idx, err := r.allocateNodeVIPIndex(context.Background(), nil)
	if err != nil {
		t.Fatalf("allocateNodeVIPIndex: %v", err)
	}
	if idx != 4 {
		t.Errorf("expected first allocation to start at NodeVIPStart=4, got %d", idx)
	}
}
