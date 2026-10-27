package aws

import (
	"context"
	"net/netip"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func requestSourceFixture(t *testing.T, extra ...client.Object) (RequestSource, *resolverEC2) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cloud := map[string]string{platform.RoleLabel: platform.RoleValue}
	machine := func(name, external string) []client.Object {
		provider := "aws:///us-west-2a/i-" + name
		m := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cluster.x-k8s.io/v1beta2", "kind": "Machine",
			"metadata": map[string]any{"name": name, "namespace": "test", "uid": name},
			"spec":     map[string]any{"providerID": provider, "clusterName": "cluster"},
			"status": map[string]any{"nodeRef": map[string]any{"name": name},
				"addresses": []any{map[string]any{"type": "ExternalIP", "address": external}, map[string]any{"type": "InternalIP", "address": "172.29.0.200"}}}}}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("node-" + name), Labels: cloud,
			Annotations: map[string]string{attachment.CalicoIPv4Address: "172.29.0.50/25"}}, Spec: corev1.NodeSpec{ProviderID: provider}}
		return []client.Object{m, node}
	}
	site := func(name, address string) *corev1.Node {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name)}}
		if address != "" {
			node.Annotations = map[string]string{attachment.CalicoIPv4Address: address}
		}
		return node
	}
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "mesh-uid"}, Data: map[string][]byte{
		tunnel.NodeTunnelAddressPrefix + "site-a": []byte("10.100.0.1/24"),
		tunnel.NodeTunnelAddressPrefix + "site-b": []byte("10.100.0.2/24"),
		tunnel.PeerEndpointPrefix + "worker":      []byte("34.1.1.1:51820"),
		tunnel.PeerEndpointPrefix + "joining":     []byte(tunnel.PeerEndpointPending),
	}}
	objects := append(machine("worker", "34.1.1.1"), machine("gateway", "44.1.1.1")...)
	objects = append(objects, site("site-a", "10.10.0.10/24"), site("site-b", "10.10.0.11/24"), site("site-windows", ""), mesh)
	objects = append(objects, extra...)
	api := &resolverEC2{}
	r := Resolver{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), API: api,
		Scope: Scope{Account: "account", Region: "us-west-2", VPCID: "vpc", SubnetID: "subnet", OwnerTag: "run", OwnerValue: "test"},
		Namespace: "test", ClusterName: "cluster", GroupID: "sg"}
	return RequestSource{Resolver: r, MeshName: "mesh"}, api
}

func felix(spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "crd.projectcalico.org/v1", "kind": "FelixConfiguration",
		"metadata": map[string]any{"name": "default"}, "spec": spec}}
}

// The controller derives a worker's gateway request from what the cluster,
// the mesh and EC2 already say, so an operator names only the gateway.
func TestGatewayRequestIsDerivedFromTheClusterMeshAndEC2(t *testing.T) {
	konnectivity := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "konnectivity-agent", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "konnectivity-agent",
			Args: []string{"--logtostderr=true", "--proxy-server-host=10.10.0.10", "--proxy-server-port=8132"}}}}}}}
	source, _ := requestSourceFixture(t, felix(map[string]any{"vxlanPort": int64(4790)}), konnectivity)
	got, err := source.Request(context.Background(), "worker", "gateway")
	if err != nil {
		t.Fatal(err)
	}
	machine := func(name, address string) attachment.Machine {
		return attachment.Machine{UID: name, NodeUID: "node-" + name, ProviderID: "aws:///us-west-2a/i-" + name, InterfaceID: "eni-i-" + name,
			NetworkID: "aws:account:us-west-2:vpc", Subnet: netip.MustParsePrefix("172.29.0.0/25"), Address: netip.MustParseAddr(address)}
	}
	addrs := func(values ...string) []netip.Addr {
		var out []netip.Addr
		for _, v := range values {
			out = append(out, netip.MustParseAddr(v))
		}
		return out
	}
	want := attachment.GatewayRequest{
		Worker: machine("worker", "172.29.0.21"), Gateway: machine("gateway", "172.29.0.9"),
		// Calico's VXLAN destinations: the site nodes Calico addresses.
		SiteTransport: addrs("10.10.0.10", "10.10.0.11"),
		// The site's tunnel endpoints source mesh traffic.
		SiteReturnSources: addrs("10.100.0.1", "10.100.0.2"),
		// Every outer tunnel endpoint: published peer endpoints and the
		// Machines' external addresses.
		Underlay: addrs("34.1.1.1", "44.1.1.1"),
		UDPPort:  4790,
		TCPPorts: []uint16{8132},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request:\n got %+v\nwant %+v", got, want)
	}
	if _, err := attachment.PlanGateway(got); err != nil {
		t.Fatal(err)
	}
}

// Without a VXLAN port of its own, Felix uses Calico's default; without
// Konnectivity there is no listener beyond the API port.
func TestGatewayRequestDefaults(t *testing.T) {
	source, _ := requestSourceFixture(t, felix(map[string]any{}))
	got, err := source.Request(context.Background(), "worker", "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if got.UDPPort != 4789 || got.TCPPorts != nil {
		t.Fatalf("port %d, tcp %v", got.UDPPort, got.TCPPorts)
	}
}

// What cannot be determined is refused, never guessed.
func TestGatewayRequestRefusesWhatItCannotDetermine(t *testing.T) {
	konnectivity := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "konnectivity-agent", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "konnectivity-agent"}}}}}}
	for name, c := range map[string]struct {
		objects         []client.Object
		worker, gateway string
		nics            bool
	}{
		"no felix configuration":     {worker: "worker", gateway: "gateway"},
		"konnectivity without port":  {objects: []client.Object{felix(map[string]any{}), konnectivity}, worker: "worker", gateway: "gateway"},
		"absent gateway":             {objects: []client.Object{felix(map[string]any{})}, worker: "worker", gateway: "missing"},
		"worker is its own gateway":  {objects: []client.Object{felix(map[string]any{})}, worker: "worker", gateway: "worker"},
		"instance with a second NIC": {objects: []client.Object{felix(map[string]any{})}, worker: "worker", gateway: "gateway", nics: true},
	} {
		t.Run(name, func(t *testing.T) {
			source, api := requestSourceFixture(t, c.objects...)
			api.secondNIC = c.nics
			if got, err := source.Request(context.Background(), c.worker, c.gateway); err == nil {
				t.Fatalf("derived %+v", got)
			}
		})
	}
}
