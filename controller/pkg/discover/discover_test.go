package discover

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// The address a joining node should dial is the cluster's own stated
// endpoint, not whichever control plane happens to be listed first.
// cluster-info in kube-public exists exactly to answer this question
// (kubeadm token joins bootstrap from it), and on an HA cluster it
// names the VIP that outlives any one control plane. Deriving the
// join address from the endpoint list instead pins every remote to
// one specific member, and its death strands them with quorum intact.
func TestAPIEndpointPrefersTheClusterInfoServer(t *testing.T) {
	clusterInfo := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-info", Namespace: "kube-public"},
		Data: map[string]string{
			"kubeconfig": `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: LS0t
    server: https://10.10.0.100:6443
  name: ""
`,
		},
	}
	got, err := APIEndpoint(context.Background(), newClient(clusterInfo))
	if err != nil {
		t.Fatalf("APIEndpoint: %v", err)
	}
	if got != "https://10.10.0.100:6443" {
		t.Fatalf("got %q, want the cluster-info server https://10.10.0.100:6443", got)
	}
}

// A loopback endpoint is a statement about every node, not an address
// anyone else can dial: it means the cluster balances node-locally
// (each node runs its own forwarder to the control planes), so the
// member list is the answer for anything rendered off-cluster, and
// returning 127.0.0.1 would bake a self-reference into a remote's
// view of the site.
func TestAPIEndpointSkipsALoopbackServer(t *testing.T) {
	clusterInfo := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-info", Namespace: "kube-public"},
		Data: map[string]string{
			"kubeconfig": `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:7445
  name: ""
`,
		},
	}
	got, err := APIEndpoint(context.Background(), newClient(clusterInfo))
	if err != nil {
		t.Fatalf("APIEndpoint: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty: a loopback endpoint is node-local by definition", got)
	}
}

// No cluster-info is an answer, not an error: the caller falls back
// to the endpoint list, which is correct on the clusters that have no
// stable endpoint to prefer.
func TestAPIEndpointAbsentIsEmptyNotError(t *testing.T) {
	got, err := APIEndpoint(context.Background(), newClient())
	if err != nil {
		t.Fatalf("APIEndpoint on a cluster without cluster-info: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
