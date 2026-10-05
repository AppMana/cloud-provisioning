package main

import (
	"context"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func dualStackScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(machineGVK.GroupVersion().WithKind(machineGVK.Kind+"List"), &unstructured.UnstructuredList{})
	return scheme
}

// A worker as a dual-stack k0s/Calico site records it: both Kubernetes
// addresses on an identity device, and the IPv6 address Calico peers on,
// which is a LAN address rather than either of them.
func dualStackWorker(name, v4, v6, calico6 string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/hostname": name},
			Annotations: map[string]string{calicoIPv4Annotation: v4 + "/32", calicoIPv6Annotation: calico6},
		},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: v4}, {Type: corev1.NodeInternalIP, Address: v6},
		}},
	}
}

func dualStackReconciler(t *testing.T, prefix6 string, objects ...client.Object) (*meshReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(dualStackScheme(t)).WithObjects(objects...).Build()
	selector, err := labels.Parse("")
	if err != nil {
		t.Fatal(err)
	}
	return &meshReconciler{
		Client: c, reader: c,
		secretNamespace: "cloud-provisioning", secretName: "peers",
		tunnelEndpointSelector: selector,
		tunnelSubnet:           "10.100.0.0/24",
		localAddressBase:       "10.100.0.1/24",
		tunnelIPv6Prefix:       prefix6,
		endpointRetention:      testRetention,
		network:                cni.Network{Name: cni.Calico, Encapsulation: cni.Encapsulated},
	}, c
}

func meshData(t *testing.T, c client.Client) map[string][]byte {
	t.Helper()
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "cloud-provisioning", Name: "peers"}, secret); err != nil {
		t.Fatal(err)
	}
	return secret.Data
}

// A dual-stack mesh gives each endpoint an IPv6 tunnel address paired
// with its IPv4 one, and records the IPv6 address the rest of the site
// forwards to when it relays: the address Calico peers on, which is
// where the site's own IPv6 routing already reaches it.
func TestADualStackEndpointGetsAnIPv6TunnelAddressAndTransitAddress(t *testing.T) {
	r, c := dualStackReconciler(t, "fd00:10:100::/96",
		dualStackWorker("w1", "10.101.0.2", "fd8f:cf26:522a::2", "fd8f:cf26:522a:0:64e3:4ca4:cc0:665c/64"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cloud-provisioning", Name: "peers"}})
	if _, err := r.reconcileTunnelEndpoints(context.Background()); err != nil {
		t.Fatal(err)
	}
	data := meshData(t, c)
	for key, want := range map[string]string{
		tunnel.NodeTunnelAddressPrefix + "w1":   "10.100.0.1/24",
		tunnel.NodeTunnelAddress6Prefix + "w1":  "fd00:10:100::a64:1/96",
		tunnel.NodeTransitAddress6Prefix + "w1": "fd8f:cf26:522a:0:64e3:4ca4:cc0:665c",
		tunnel.NodeAddressesPrefix + "w1":       "10.101.0.2,fd8f:cf26:522a::2",
	} {
		if got := string(data[key]); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// A single-stack mesh carries no IPv6 tunnel state, and one that stops
// being dual-stack withdraws what it had.
func TestASingleStackMeshCarriesNoIPv6TunnelState(t *testing.T) {
	r, c := dualStackReconciler(t, "",
		dualStackWorker("w1", "10.101.0.2", "fd8f:cf26:522a::2", "fd8f:cf26:522a:0:64e3:4ca4:cc0:665c/64"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cloud-provisioning", Name: "peers"}, Data: map[string][]byte{
			tunnel.NodeTunnelAddress6Prefix + "w1":  []byte("fd00:10:100::a64:1/96"),
			tunnel.NodeTransitAddress6Prefix + "w1": []byte("fd8f:cf26:522a:0:64e3:4ca4:cc0:665c"),
		}})
	if _, err := r.reconcileTunnelEndpoints(context.Background()); err != nil {
		t.Fatal(err)
	}
	data := meshData(t, c)
	for _, key := range []string{tunnel.NodeTunnelAddress6Prefix + "w1", tunnel.NodeTransitAddress6Prefix + "w1"} {
		if v, ok := data[key]; ok {
			t.Errorf("%s = %q on a single-stack mesh", key, v)
		}
	}
	if got := string(data[tunnel.NodeTunnelAddressPrefix+"w1"]); got != "10.100.0.1/24" {
		t.Errorf("IPv4 tunnel address = %q", got)
	}
}

// A node that has left the cluster takes its IPv6 entries with it,
// exactly as it takes the IPv4 ones.
func TestADepartedNodeWithdrawsItsIPv6Entries(t *testing.T) {
	data := map[string][]byte{
		tunnel.NodePublicKeyPrefix + "gone":       []byte("GONEKEY"),
		tunnel.NodeTunnelAddressPrefix + "gone":   []byte("10.100.0.2/24"),
		tunnel.NodeTunnelAddress6Prefix + "gone":  []byte("fd00:10:100::a64:2/96"),
		tunnel.NodeTransitAddress6Prefix + "gone": []byte("fd8f:cf26:522a::a:12"),
		tunnel.NodePublicKeyPrefix + "w1":         []byte("W1KEY"),
		tunnel.NodeTunnelAddressPrefix + "w1":     []byte("10.100.0.1/24"),
	}
	pruneDeparted(data, meshMembership{endpoints: map[string]bool{"w1": true}, siteNodes: map[string]bool{}}, testNow, testRetention, nil)
	for _, key := range []string{tunnel.NodeTunnelAddress6Prefix + "gone", tunnel.NodeTransitAddress6Prefix + "gone"} {
		if _, ok := data[key]; ok {
			t.Errorf("%s survived its node leaving the cluster", key)
		}
	}
}

// A remote is told which addresses its CNI peers on, one per family.
// Calico's IPv6 autodetection on a remote searches the site's prefix and
// finds nothing there; with a stored address it keeps that one, and
// without one its node agent refuses to start.
func TestARemoteIsGivenACNIAddressInEachFamily(t *testing.T) {
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	machine.SetName("remote1")
	machine.SetNamespace("cloud-provisioning")
	machine.SetLabels(map[string]string{"cloud-provisioning.appmana.com/role": "cloud-worker"})
	machine.SetAnnotations(map[string]string{
		"cloud-provisioning.appmana.com/wireguard-addr4": "10.100.0.128/24",
		"cloud-provisioning.appmana.com/wireguard-addr6": "fd00:10:100::a64:80/96",
	})
	if err := unstructured.SetNestedField(machine.Object, "remote1", "status", "nodeRef", "name"); err != nil {
		t.Fatal(err)
	}
	r, c := dualStackReconciler(t, "fd00:10:100::/96", &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "remote1"}}, machine)
	if err := r.ensureCNINodeAddressForMachine(context.Background(), machine); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Node{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "remote1"}, got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{calicoIPv4Annotation: "10.100.0.128/32", calicoIPv6Annotation: "fd00:10:100::a64:80/128"} {
		if got.Annotations[key] != want {
			t.Errorf("%s = %q, want %q", key, got.Annotations[key], want)
		}
	}
}

// A transit speaker names this node as the next hop in each family it
// advertises, so it needs every one of the node's addresses, not only
// the primary one the downward API's hostIP carries.
func TestTheSiteDialerIsGivenEveryNodeAddress(t *testing.T) {
	r, c := dualStackReconciler(t, "fd00:10:100::/96")
	r.dialerDaemonSetName = "tunnel-dialer"
	r.dialerImage = "dialer@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	r.transitBGPPort = 1790
	if err := r.ensureDialerDaemonSet(context.Background()); err != nil {
		t.Fatal(err)
	}
	ds := daemonSet(t, c, "tunnel-dialer")
	container := ds.Spec.Template.Spec.Containers[0]
	found := false
	for _, env := range container.Env {
		if env.Name == "NODE_IPS" && env.ValueFrom != nil && env.ValueFrom.FieldRef != nil && env.ValueFrom.FieldRef.FieldPath == "status.hostIPs" {
			found = true
		}
	}
	if !found {
		t.Errorf("the dialer has no NODE_IPS from status.hostIPs: %+v", container.Env)
	}
	want := "--transit-bgp-next-hop=$(NODE_IPS)"
	has := false
	for _, arg := range container.Args {
		if arg == want {
			has = true
		}
	}
	if !has {
		t.Errorf("dialer args %v lack %s", container.Args, want)
	}
}

func daemonSet(t *testing.T, c client.Client, name string) *appsv1.DaemonSet {
	t.Helper()
	ds := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "cloud-provisioning", Name: name}, ds); err != nil {
		t.Fatal(err)
	}
	return ds
}

// The IPv6 tunnel prefix applies only to a pod network that carries
// IPv6, and one that cannot hold an IPv4 address in its host bits is a
// configuration error caught at startup.
func TestTheIPv6TunnelPrefixFollowsThePodNetwork(t *testing.T) {
	for _, tc := range []struct {
		configured string
		dualStack  bool
		want       string
		fails      bool
	}{
		{"fd00:10:100::/96", true, "fd00:10:100::/96", false},
		{"fd00:10:100::/96", false, "", false},
		{"", true, "", false},
		{"fd00:10:100::/112", true, "", true},
		{"10.100.0.0/24", false, "", true},
	} {
		got, err := effectiveTunnelIPv6Prefix(tc.configured, tc.dualStack)
		if (err != nil) != tc.fails || got != tc.want {
			t.Errorf("effectiveTunnelIPv6Prefix(%q, %v) = %q, %v", tc.configured, tc.dualStack, got, err)
		}
	}
}
