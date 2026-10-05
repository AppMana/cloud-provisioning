package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const fixture = "../../../controller/pkg/cni/testdata/k0s-v1.36.4-calico-v3.32.1-bird-dualstack/"

// observedCluster loads the read-only capture of the deployed dual-stack
// k0s/Calico site: its pools, block affinities, nodes and calico-node
// DaemonSet.
func observedCluster(t *testing.T, mutate func(ds *appsv1.DaemonSet)) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"IPPool", "BlockAffinity", "IPAMBlock", "BGPPeer"} {
		gvk := schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: kind}
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	var objects []client.Object
	for _, name := range []string{"ippools.json", "blocks.json"} {
		raw, err := os.ReadFile(fixture + name)
		if err != nil {
			t.Fatal(err)
		}
		var list unstructured.UnstructuredList
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatal(err)
		}
		for i := range list.Items {
			objects = append(objects, &list.Items[i])
		}
	}
	raw, err := os.ReadFile(fixture + "nodes.json")
	if err != nil {
		t.Fatal(err)
	}
	var nodes corev1.NodeList
	if err := json.Unmarshal(raw, &nodes); err != nil {
		t.Fatal(err)
	}
	for i := range nodes.Items {
		objects = append(objects, &nodes.Items[i])
	}
	raw, err = os.ReadFile(fixture + "calico-node.json")
	if err != nil {
		t.Fatal(err)
	}
	ds := &appsv1.DaemonSet{}
	if err := json.Unmarshal(raw, ds); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(ds)
	}
	objects = append(objects, ds, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
		Spec:       corev1.ServiceSpec{ClusterIPs: []string{"10.101.4.1", "fd8f:cf26:522a:4::1"}},
	})
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func byName(results []Check) map[string]Check {
	out := map[string]Check{}
	for _, r := range results {
		out[r.Name] = r
	}
	return out
}

// The deployed cluster as captured: a native dual-stack Calico network
// the product recognises, whose blocks keep their pod sources, whose
// address autodetection cannot select a remote's addresses, and whose
// ranges stay clear of the tunnel's. Its pods' 1450-byte MTU is larger
// than the 1420 bytes the tunnel carries over a 1500-byte underlay, and
// that is the one required change.
func TestTheObservedDeploymentNeedsOnlyItsPodMTULowered(t *testing.T) {
	results, err := Preconditions(context.Background(), observedCluster(t, nil), Options{TunnelMTU: 1420})
	if err != nil {
		t.Fatal(err)
	}
	got := byName(results)
	for _, name := range []string{"network", "dual-stack", "masquerade", "ipv4-autodetection", "ipv6-autodetection", "tunnel-ranges"} {
		if c, ok := got[name]; !ok || !c.OK {
			t.Errorf("%s: %+v", name, c)
		}
	}
	mtu := got["pod-mtu"]
	if mtu.OK || !mtu.Required || !strings.Contains(mtu.Detail, "1450") || !strings.Contains(mtu.Detail, "1420") {
		t.Errorf("pod-mtu = %+v, want a required failure naming 1450 and 1420", mtu)
	}
	if Passed(results) {
		t.Error("the precondition set passed with a pod MTU larger than the tunnel's")
	}
	// Lowered to the tunnel's MTU, nothing required remains.
	results, err = Preconditions(context.Background(), observedCluster(t, func(ds *appsv1.DaemonSet) {
		setCNIMTU(ds, 1420)
	}), Options{TunnelMTU: 1420})
	if err != nil {
		t.Fatal(err)
	}
	if !Passed(results) {
		t.Errorf("a 1420-byte pod MTU still fails: %+v", results)
	}
}

// Autodetection that can find an address on a remote fights the address
// the product publishes for it: a remote's calico-node keeps a stored
// address only when its own detection finds nothing.
func TestAutodetectionThatCanSelectARemoteAddressIsRefused(t *testing.T) {
	for method, ok := range map[string]bool{
		"first-found":                false,
		"kubernetes-internal-ip":     false,
		"can-reach=10.101.0.1":       false,
		"cidr=10.0.0.0/8":            false, // covers the IPv4 tunnel range
		"cidr=10.101.0.0/24":         true,
		"interface=eno1":             false,
		"skip-interface=cldt.*,eth9": false,
	} {
		results, err := Preconditions(context.Background(), observedCluster(t, func(ds *appsv1.DaemonSet) {
			setEnv(ds, "IP_AUTODETECTION_METHOD", method)
		}), Options{TunnelMTU: 1420})
		if err != nil {
			t.Fatal(err)
		}
		if c := byName(results)["ipv4-autodetection"]; c.OK != ok {
			t.Errorf("method %q: %+v, want ok=%v", method, c, ok)
		}
	}
}

func setEnv(ds *appsv1.DaemonSet, name, value string) {
	for i := range ds.Spec.Template.Spec.Containers {
		c := &ds.Spec.Template.Spec.Containers[i]
		if c.Name != "calico-node" {
			continue
		}
		for j := range c.Env {
			if c.Env[j].Name == name {
				c.Env[j].Value = value
			}
		}
	}
}

func setCNIMTU(ds *appsv1.DaemonSet, mtu int) {
	for i := range ds.Spec.Template.Spec.InitContainers {
		c := &ds.Spec.Template.Spec.InitContainers[i]
		for j := range c.Env {
			if c.Env[j].Name == "CNI_NETWORK_CONFIG" {
				c.Env[j].Value = strings.Replace(c.Env[j].Value, `"mtu": 1450`, `"mtu": `+itoa(mtu), 1)
			}
		}
	}
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}
