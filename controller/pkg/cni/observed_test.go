package cni

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Captured from single-NIC VM and real CAPA AWS bringups on 2026-09-05/06.
// This tests interpretation of real resources, not packet reachability.
func TestObservedK0sBundledCalico(t *testing.T) {
	for _, fixture := range []string{"k0s-v1.34.1-calico-v3.29.6", "k0s-v1.34.1-calico-v3.29.6-aws"} {
		t.Run(fixture, func(t *testing.T) {
			var objects []client.Object
			var observedNodes []string
			for _, name := range []string{"ippools.json", "blocks.json"} {
				raw, err := os.ReadFile("testdata/" + fixture + "/" + name)
				if err != nil {
					t.Fatal(err)
				}
				var list unstructured.UnstructuredList
				if err := json.Unmarshal(raw, &list); err != nil {
					t.Fatal(err)
				}
				for i := range list.Items {
					objects = append(objects, &list.Items[i])
					if node, found, _ := unstructured.NestedString(list.Items[i].Object, "spec", "node"); found {
						observedNodes = append(observedNodes, node)
					}
				}
			}
			reader := newClient(objects...)
			network, err := Detect(context.Background(), reader)
			if err != nil {
				t.Fatal(err)
			}
			if network.Name != Calico || network.Encapsulation != Encapsulated {
				t.Fatalf("observed bundled Calico detected as %+v", network)
			}
			for _, node := range observedNodes {
				prefixes, err := network.PrefixesFor(context.Background(), reader, node)
				if err != nil || len(prefixes) != 0 {
					t.Fatalf("VXLAN must route by node address, without advertising native pod blocks for %s: %v %v", node, prefixes, err)
				}
			}
		})
	}
}

func TestObservedK0sBundledKubeRouter(t *testing.T) {
	for _, fixture := range []string{"k0s-v1.34.1-kube-router-v2.6.1", "k0s-v1.36.2-kube-router"} {
		t.Run(fixture, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/" + fixture + "/daemonset.json")
			if err != nil {
				t.Fatal(err)
			}
			ds := &unstructured.Unstructured{}
			if err := json.Unmarshal(raw, ds); err != nil {
				t.Fatal(err)
			}
			raw, err = os.ReadFile("testdata/" + fixture + "/nodes.json")
			if err != nil {
				t.Fatal(err)
			}
			var nodes unstructured.UnstructuredList
			if err := json.Unmarshal(raw, &nodes); err != nil {
				t.Fatal(err)
			}
			objects := []client.Object{ds}
			for i := range nodes.Items {
				objects = append(objects, &nodes.Items[i])
			}
			reader := newClient(objects...)
			network, err := Detect(context.Background(), reader)
			if err != nil {
				t.Fatal(err)
			}
			if network.Name != KubeRouter || network.Encapsulation != Native {
				t.Fatalf("observed kube-router detected as %+v", network)
			}
			for _, node := range nodes.Items {
				prefixes, err := network.PrefixesFor(context.Background(), reader, node.GetName())
				if err != nil {
					t.Fatal(err)
				}
				cidr, _, _ := unstructured.NestedString(node.Object, "spec", "podCIDR")
				if len(prefixes) != 1 || prefixes[0].String() != cidr {
					t.Fatalf("native pod route for %s: got %v, want observed %s", node.GetName(), prefixes, cidr)
				}
			}
		})
	}
}

func TestObservedOKDManagedOVN(t *testing.T) {
	raw, err := os.ReadFile("testdata/okd-4.21.0-scos.11/network.json")
	if err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw, obj); err != nil {
		t.Fatal(err)
	}
	// The distribution's selected network takes precedence over unused old
	// Calico resources, which can remain after a network migration.
	reader := newClient(obj, ipPool("old", "10.244.0.0/16", "Never", "Never"))
	network, err := Detect(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if network.Name != "ovn-kubernetes" || network.Encapsulation != Encapsulated {
		t.Fatalf("observed managed OVN detected as %+v", network)
	}
	prefixes, err := network.PrefixesFor(context.Background(), reader, "cp3")
	if err != nil || len(prefixes) != 0 {
		t.Fatalf("encapsulated OVN required native pod prefixes: %v %v", prefixes, err)
	}
}

func TestObservedK3sEmbeddedFlannel(t *testing.T) {
	raw, err := os.ReadFile("testdata/k3s-v1.34.1-flannel/nodes.json")
	if err != nil {
		t.Fatal(err)
	}
	var nodes unstructured.UnstructuredList
	if err := json.Unmarshal(raw, &nodes); err != nil {
		t.Fatal(err)
	}
	var objects []client.Object
	for i := range nodes.Items {
		objects = append(objects, &nodes.Items[i])
	}
	// The observed k3s bundle exposes Flannel through Node annotations,
	// without a standalone Flannel DaemonSet or configuration ConfigMap.
	reader := newClient(objects...)
	network, err := Detect(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if network.Name != Flannel || network.Encapsulation != Encapsulated {
		t.Fatalf("observed embedded Flannel detected as %+v", network)
	}
	for _, node := range nodes.Items {
		prefixes, err := network.PrefixesFor(context.Background(), reader, node.GetName())
		if err != nil || len(prefixes) != 0 {
			t.Fatalf("encapsulated Flannel required native prefixes for %s: %v %v", node.GetName(), prefixes, err)
		}
	}
}

func TestObservedRKE2BundledCanal(t *testing.T) {
	raw, err := os.ReadFile("testdata/rke2-v1.34.1-canal/resources.json")
	if err != nil {
		t.Fatal(err)
	}
	var resources unstructured.UnstructuredList
	if err := json.Unmarshal(raw, &resources); err != nil {
		t.Fatal(err)
	}
	var objects []client.Object
	for i := range resources.Items {
		objects = append(objects, &resources.Items[i])
	}
	reader := newClient(objects...)
	network, err := Detect(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	// The real Canal installation includes a Calico pool with Never/Never
	// encapsulation. Its Flannel transport still uses VXLAN.
	if network.Name != Flannel || network.Encapsulation != Encapsulated {
		t.Fatalf("observed Canal transport detected as %+v", network)
	}
	prefixes, err := network.PrefixesFor(context.Background(), reader, "cp")
	if err != nil || len(prefixes) != 0 {
		t.Fatalf("encapsulated Canal required native pod prefixes: %v %v", prefixes, err)
	}
}

func TestObservedKubeadmNativeCalicoBlocks(t *testing.T) {
	raw, err := os.ReadFile("testdata/kubeadm-v1.34.0-calico/resources.json")
	if err != nil {
		t.Fatal(err)
	}
	var resources unstructured.UnstructuredList
	if err := json.Unmarshal(raw, &resources); err != nil {
		t.Fatal(err)
	}
	var objects []client.Object
	for i := range resources.Items {
		objects = append(objects, &resources.Items[i])
	}
	reader := newClient(objects...)
	network, err := Detect(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if network.Name != Calico || network.Encapsulation != Native {
		t.Fatalf("observed native Calico detected as %+v", network)
	}
	for node, cidr := range map[string]string{"remote1": "10.244.159.0/26", "remote2": "10.244.133.0/26"} {
		prefixes, err := network.PrefixesFor(context.Background(), reader, node)
		if err != nil || len(prefixes) != 1 || prefixes[0] != netip.MustParsePrefix(cidr) {
			t.Fatalf("observed blocks for %s: got %v, %v; want %s", node, prefixes, err, cidr)
		}
	}
}
