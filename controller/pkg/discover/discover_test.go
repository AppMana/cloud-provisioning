package discover

import (
	"context"
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	_ = scheme.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func TestAPIServersReadsEveryControlPlane(t *testing.T) {
	port := int32(6443)
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kubernetes",
			Namespace: metav1.NamespaceDefault,
			Labels:    map[string]string{discoveryv1.LabelServiceName: "kubernetes"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Port: &port}},
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.0.3"}},
			{Addresses: []string{"10.0.0.1"}},
			{Addresses: []string{"10.0.0.2"}},
		},
	}
	got, err := APIServers(context.Background(), newClient(slice))
	if err != nil {
		t.Fatalf("APIServers: %v", err)
	}
	want := []string{"10.0.0.1:6443", "10.0.0.2:6443", "10.0.0.3:6443"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A node's allocated block is not the cluster's range: joining nodes
// talk to every node's pods, so the accept list needs the whole range.
func TestPodCIDRsWidenPerNodeBlocksToTheClusterRange(t *testing.T) {
	nodes := []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.1.0/24"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.2.0/24"}}},
	}
	got, err := podCIDRsFromNodes(context.Background(), newClient(nodes...))
	if err != nil {
		t.Fatalf("podCIDRsFromNodes: %v", err)
	}
	if len(got) != 1 || got[0] != "10.244.0.0/16" {
		t.Fatalf("got %v, want [10.244.0.0/16]", got)
	}
}

func TestPodCIDRsFromCiliumConfigMap(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium-config"},
		Data: map[string]string{
			"cluster-pool-ipv4-cidr": "10.0.0.0/8",
			"cluster-pool-ipv6-cidr": "fd00::/104",
		},
	}
	got, err := podCIDRsFromCilium(context.Background(), newClient(cm))
	if err != nil {
		t.Fatalf("podCIDRsFromCilium: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want both families", got)
	}
}

func TestPodCIDRsFromFlannelConfigMap(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-flannel", Name: "kube-flannel-cfg"},
		Data: map[string]string{
			"net-conf.json": `{"Network": "10.42.0.0/16", "Backend": {"Type": "vxlan"}}`,
		},
	}
	got, err := podCIDRsFromFlannel(context.Background(), newClient(cm))
	if err != nil {
		t.Fatalf("podCIDRsFromFlannel: %v", err)
	}
	if len(got) != 1 || got[0] != "10.42.0.0/16" {
		t.Fatalf("got %v, want [10.42.0.0/16]", got)
	}
}

func TestNodeAddressRangesComeFromInternalIPs(t *testing.T) {
	nodes := []client.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "a"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "203.0.113.7"},
				{Type: corev1.NodeInternalIP, Address: "10.20.30.40"},
				{Type: corev1.NodeInternalIP, Address: "fd00:1234::5"},
			}},
		},
	}
	got, err := NodeAddressRanges(context.Background(), newClient(nodes...))
	if err != nil {
		t.Fatalf("NodeAddressRanges: %v", err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("10.20.30.0/24"),
		netip.MustParsePrefix("fd00:1234::/64"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// An external address must never be mistaken for the cluster's own
	// range: allocating a remote node an address out of 203.0.113.0/24
	// would put it outside the address space the CNI peers on.
	for _, p := range got {
		if p.Contains(netip.MustParseAddr("203.0.113.7")) {
			t.Fatalf("range %s came from an ExternalIP", p)
		}
	}
}

func TestServiceCIDRReadFromAllocatorRejection(t *testing.T) {
	// The exact shape of the API server's rejection, which is the only
	// place a pre-1.31 cluster states its service range.
	const msg = `Service "probe" is invalid: spec.clusterIPs: Invalid value: []string{"192.0.2.1"}: ` +
		`failed to allocate IP 192.0.2.1: the provided IP (192.0.2.1) is not in the valid range. ` +
		`The range of valid IPs is 10.96.0.0/12`
	got, err := parseServiceRange(msg)
	if err != nil {
		t.Fatalf("parseServiceRange: %v", err)
	}
	if got != "10.96.0.0/12" {
		t.Fatalf("got %q, want 10.96.0.0/12", got)
	}
}

// A cluster can carry several pools, and a pod may be allocated from
// any of the enabled ones, so every enabled pool has to reach the
// accept list. A disabled pool allocates nothing and must not.
func TestPodCIDRsCollectsEveryEnabledCalicoPool(t *testing.T) {
	pools := &unstructured.UnstructuredList{}
	pools.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "crd.projectcalico.org", Version: "v1", Kind: "IPPoolList",
	})
	for _, p := range []struct {
		name     string
		cidr     string
		disabled bool
	}{
		{"default-ipv4-ippool", "10.244.0.0/16", false},
		{"second-ipv4-ippool", "10.245.0.0/16", false},
		{"default-ipv6-ippool", "fd00:10:244::/56", false},
		{"retired-ippool", "10.240.0.0/16", true},
	} {
		item := unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "crd.projectcalico.org/v1",
			"kind":       "IPPool",
			"metadata":   map[string]any{"name": p.name},
			"spec":       map[string]any{"cidr": p.cidr, "disabled": p.disabled},
		}}
		pools.Items = append(pools.Items, item)
	}

	s := runtime.NewScheme()
	_ = scheme.AddToScheme(s)
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: "crd.projectcalico.org", Version: "v1", Kind: "IPPoolList",
	}, &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: "crd.projectcalico.org", Version: "v1", Kind: "IPPool",
	}, &unstructured.Unstructured{})
	objs := make([]client.Object, 0, len(pools.Items))
	for i := range pools.Items {
		objs = append(objs, &pools.Items[i])
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()

	got, err := podCIDRsFromCalico(context.Background(), c)
	if err != nil {
		t.Fatalf("podCIDRsFromCalico: %v", err)
	}
	want := map[string]bool{"10.244.0.0/16": true, "10.245.0.0/16": true, "fd00:10:244::/56": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want the three enabled pools", got)
	}
	for _, cidr := range got {
		if !want[cidr] {
			t.Errorf("%s is not an enabled pool", cidr)
		}
	}
}
