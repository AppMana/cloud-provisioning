package cni

import (
	"context"
	"net/netip"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newClient(objs ...client.Object) client.Reader {
	s := runtime.NewScheme()
	_ = scheme.AddToScheme(s)
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "crd.projectcalico.org", Version: "v1", Kind: "IPPool"},
		{Group: "crd.projectcalico.org", Version: "v1", Kind: "BlockAffinity"},
		{Group: "cilium.io", Version: "v2", Kind: "CiliumNode"},
	} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func ipPool(name, cidr, ipip, vxlan string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "crd.projectcalico.org/v1",
		"kind":       "IPPool",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"cidr": cidr, "ipipMode": ipip, "vxlanMode": vxlan},
	}}
}

func blockAffinity(name, node, cidr, state, deleted string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "crd.projectcalico.org/v1",
		"kind":       "BlockAffinity",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"node": node, "cidr": cidr, "state": state, "deleted": deleted},
	}}
}

func configMap(ns, name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: data}
}

// The encapsulation decides whether a pod prefix is needed at all, so
// every mode of every network has to be read correctly from what that
// network itself records.
func TestDetect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		objs    []client.Object
		wantCNI string
		wantEnc Encapsulation
	}{
		{
			name:    "calico routes natively when no pool encapsulates",
			objs:    []client.Object{ipPool("default-ipv4-ippool", "10.244.0.0/16", "Never", "Never")},
			wantCNI: Calico, wantEnc: Native,
		},
		{
			name:    "calico with vxlan always",
			objs:    []client.Object{ipPool("default-ipv4-ippool", "10.244.0.0/16", "Never", "Always")},
			wantCNI: Calico, wantEnc: Encapsulated,
		},
		{
			name:    "calico with ipip always",
			objs:    []client.Object{ipPool("default-ipv4-ippool", "10.244.0.0/16", "Always", "Never")},
			wantCNI: Calico, wantEnc: Encapsulated,
		},
		{
			// A node across a tunnel is never on the same subnet, so
			// CrossSubnet always encapsulates for the peers here.
			name:    "calico with cross-subnet encapsulates for a remote node",
			objs:    []client.Object{ipPool("default-ipv4-ippool", "10.244.0.0/16", "CrossSubnet", "Never")},
			wantCNI: Calico, wantEnc: Encapsulated,
		},
		{
			name: "calico with one encapsulating pool among several",
			objs: []client.Object{
				ipPool("a", "10.244.0.0/16", "Never", "Never"),
				ipPool("b", "10.245.0.0/16", "Never", "Always"),
			},
			wantCNI: Calico, wantEnc: Encapsulated,
		},
		{
			name:    "cilium native",
			objs:    []client.Object{configMap("kube-system", "cilium-config", map[string]string{"routing-mode": "native", "ipv4-native-routing-cidr": "10.0.0.0/8"})},
			wantCNI: Cilium, wantEnc: Native,
		},
		{
			name:    "cilium tunnel with geneve",
			objs:    []client.Object{configMap("kube-system", "cilium-config", map[string]string{"routing-mode": "tunnel", "tunnel-protocol": "geneve"})},
			wantCNI: Cilium, wantEnc: Encapsulated,
		},
		{
			name:    "cilium defaults to tunnel when the mode is unset",
			objs:    []client.Object{configMap("kube-system", "cilium-config", map[string]string{"cluster-pool-ipv4-cidr": "10.0.0.0/8"})},
			wantCNI: Cilium, wantEnc: Encapsulated,
		},
		{
			name:    "cilium with the older disabled-tunnel key",
			objs:    []client.Object{configMap("kube-system", "cilium-config", map[string]string{"tunnel": "disabled"})},
			wantCNI: Cilium, wantEnc: Native,
		},
		{
			name:    "flannel host-gw routes natively",
			objs:    []client.Object{configMap("kube-flannel", "kube-flannel-cfg", map[string]string{"net-conf.json": `{"Network":"10.42.0.0/16","Backend":{"Type":"host-gw"}}`})},
			wantCNI: Flannel, wantEnc: Native,
		},
		{
			name:    "flannel vxlan encapsulates",
			objs:    []client.Object{configMap("kube-flannel", "kube-flannel-cfg", map[string]string{"net-conf.json": `{"Network":"10.42.0.0/16","Backend":{"Type":"vxlan"}}`})},
			wantCNI: Flannel, wantEnc: Encapsulated,
		},
		{
			name:    "flannel wireguard encapsulates",
			objs:    []client.Object{configMap("kube-system", "kube-flannel-cfg", map[string]string{"net-conf.json": `{"Network":"10.42.0.0/16","EnableIPv6":true,"IPv6Network":"fd00::/48","Backend":{"Type":"wireguard"}}`})},
			wantCNI: Flannel, wantEnc: Encapsulated,
		},
		{
			name: "kube-router with the overlay turned off",
			objs: []client.Object{&appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-router"},
				Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "kube-router", Args: []string{"--run-router=true", "--enable-overlay=false"}}},
				}}},
			}},
			wantCNI: KubeRouter, wantEnc: Native,
		},
		{
			// The overlay is on unless it is turned off, so an absent
			// flag must not be read as native.
			name: "kube-router defaults to an overlay",
			objs: []client.Object{&appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-router"},
				Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "kube-router", Args: []string{"--run-router=true"}}},
				}}},
			}},
			wantCNI: KubeRouter, wantEnc: Encapsulated,
		},
		{
			name:    "an unrecognised network is reported as such",
			objs:    nil,
			wantCNI: Unrecognised, wantEnc: Unknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Detect(context.Background(), newClient(tc.objs...))
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if got.Name != tc.wantCNI || got.Encapsulation != tc.wantEnc {
				t.Errorf("got %s/%s (%s), want %s/%s", got.Name, got.Encapsulation, got.Detail, tc.wantCNI, tc.wantEnc)
			}
		})
	}
}

// An encapsulated network addresses its packets to the node, so no pod
// prefix belongs in the accept list at all. Permitting one there would
// take traffic the tunnel has no reason to carry.
func TestPrefixesFor_EncapsulatedNeedsNone(t *testing.T) {
	n := Network{Name: Calico, Encapsulation: Encapsulated}
	got, err := n.PrefixesFor(context.Background(), newClient(), "worker-1")
	if err != nil {
		t.Fatalf("PrefixesFor: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

// An unrecognised network must refuse rather than guess: too narrow
// drops traffic silently, too wide takes traffic meant for elsewhere.
func TestPrefixesFor_UnknownRefusesToGuess(t *testing.T) {
	n := Network{Name: Unrecognised, Encapsulation: Unknown}
	if _, err := n.PrefixesFor(context.Background(), newClient(), "worker-1"); err == nil {
		t.Fatal("expected an error for an unrecognised network, got nil")
	}
}

// Calico records the blocks it routes to each node. Only confirmed,
// undeleted blocks are in use, and a block belonging to another node
// must never be permitted through this peer.
func TestPrefixesFor_CalicoReadsThatNodesBlocksOnly(t *testing.T) {
	c := newClient(
		blockAffinity("a1", "worker-1", "10.244.1.0/26", "confirmed", "false"),
		blockAffinity("a2", "worker-1", "10.244.2.0/26", "confirmed", "false"),
		blockAffinity("a3", "worker-1", "2001:db8:1::/122", "confirmed", "false"),
		blockAffinity("a4", "worker-1", "10.244.9.0/26", "confirmed", "true"),
		blockAffinity("a5", "worker-1", "10.244.8.0/26", "pending", "false"),
		blockAffinity("b1", "worker-2", "10.244.3.0/26", "confirmed", "false"),
	)
	n := Network{Name: Calico, Encapsulation: Native}
	got, err := n.PrefixesFor(context.Background(), c, "worker-1")
	if err != nil {
		t.Fatalf("PrefixesFor: %v", err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("10.244.1.0/26"),
		netip.MustParsePrefix("10.244.2.0/26"),
		netip.MustParsePrefix("2001:db8:1::/122"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, w := range want {
		var found bool
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from %v", w, got)
		}
	}
	for _, g := range got {
		if g == netip.MustParsePrefix("10.244.3.0/26") {
			t.Error("worker-2's block was permitted through worker-1")
		}
		if g == netip.MustParsePrefix("10.244.9.0/26") {
			t.Error("a deleted block was permitted")
		}
		if g == netip.MustParsePrefix("10.244.8.0/26") {
			t.Error("a pending block was permitted")
		}
	}
}

func TestPrefixesFor_CiliumReadsItsOwnNodeRecord(t *testing.T) {
	ciliumNode := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNode",
		"metadata":   map[string]any{"name": "worker-1"},
		"spec":       map[string]any{"ipam": map[string]any{"podCIDRs": []any{"10.0.1.0/24"}}},
	}}
	n := Network{Name: Cilium, Encapsulation: Native}
	got, err := n.PrefixesFor(context.Background(), newClient(ciliumNode), "worker-1")
	if err != nil {
		t.Fatalf("PrefixesFor: %v", err)
	}
	if len(got) != 1 || got[0] != netip.MustParsePrefix("10.0.1.0/24") {
		t.Errorf("got %v, want [10.0.1.0/24]", got)
	}
}

// A network without its own address management routes by the block the
// controller manager allocated.
func TestPrefixesFor_FallsBackToTheNodesAllocation(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.42.1.0/24", "fd00:42:1::/64"}},
	}
	n := Network{Name: Flannel, Encapsulation: Native}
	got, err := n.PrefixesFor(context.Background(), newClient(node), "worker-1")
	if err != nil {
		t.Fatalf("PrefixesFor: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want both families", got)
	}
}

// Nothing may report the cluster's whole range: that is the union of
// every node's blocks, and permitting it on one peer takes the traffic
// belonging to all the others.
func TestPrefixesFor_NeverReturnsAClusterWideRange(t *testing.T) {
	c := newClient(
		blockAffinity("a1", "worker-1", "10.244.1.0/26", "confirmed", "false"),
		blockAffinity("b1", "worker-2", "10.244.3.0/26", "confirmed", "false"),
	)
	n := Network{Name: Calico, Encapsulation: Native}
	got, err := n.PrefixesFor(context.Background(), c, "worker-1")
	if err != nil {
		t.Fatalf("PrefixesFor: %v", err)
	}
	for _, p := range got {
		if p.Bits() < 24 {
			t.Errorf("%s is wider than a node block", p)
		}
		if p.Contains(netip.MustParseAddr("10.244.3.1")) {
			t.Errorf("%s covers another node's block", p)
		}
	}
}
