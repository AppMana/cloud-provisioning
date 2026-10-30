package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
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
func observedCluster(t *testing.T, mutate func(ds *appsv1.DaemonSet), extra ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"IPPool", "BlockAffinity", "IPAMBlock", "BGPPeer", "BGPConfiguration"} {
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
	objects = append(objects, extra...)
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
// the product recognises, whose blocks keep their pod sources and whose
// ranges stay clear of the tunnel's. Its pods' 1450-byte MTU is larger than
// the 1420 bytes the tunnel carries over a 1500-byte underlay, and that is
// the one required change: Calico's addresses on cloud nodes and its BGP
// mesh are the product's, not settings the operator makes.
func TestTheObservedDeploymentNeedsOnlyItsPodMTULowered(t *testing.T) {
	results, err := Preconditions(context.Background(), observedCluster(t, nil), Options{TunnelMTU: 1420})
	if err != nil {
		t.Fatal(err)
	}
	got := byName(results)
	for _, name := range []string{"network", "dual-stack", "masquerade", "tunnel-ranges", "bgp-peers"} {
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
	results, err = Preconditions(context.Background(), observedCluster(t, func(ds *appsv1.DaemonSet) {
		setCNIMTU(ds, 1420)
	}), Options{TunnelMTU: 1420})
	if err != nil {
		t.Fatal(err)
	}
	if !Passed(results) {
		t.Errorf("the required settings still fail: %+v", results)
	}
}

func bgpPeer(name, nodeSelector string) *unstructured.Unstructured {
	spec := map[string]any{"peerIP": "10.2.0.1"}
	if nodeSelector != "" {
		spec["nodeSelector"] = nodeSelector
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "crd.projectcalico.org/v1", "kind": "BGPPeer",
		"metadata": map[string]any{"name": name}, "spec": spec}}
}

// A peer the operator configured, such as a site router, must leave cloud
// nodes out, since the product does not change it.
func TestARouterPeerMustLeaveCloudNodesOut(t *testing.T) {
	for selector, ok := range map[string]bool{
		"":      false,
		"all()": false,
		"!has(cloud-provisioning.appmana.com/role)":                                true,
		"!has(cloud-provisioning.appmana.com/role) && kubernetes.io/os == 'linux'": true,
		"!has(cloud-provisioning.appmana.com/role) || rack == 'a'":                 false,
	} {
		results, err := Preconditions(context.Background(), observedCluster(t, nil, bgpPeer("router", selector)), Options{TunnelMTU: 1420})
		if err != nil {
			t.Fatal(err)
		}
		if c := byName(results)["bgp-peers"]; c.OK != ok || !c.Required {
			t.Errorf("nodeSelector %q: %+v, want ok=%v", selector, c, ok)
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

func TestPodMTURequiresAnExplicitUsableBudget(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		ok          bool
	}{
		{"missing", "", false},
		{"null", `,"mtu":null`, false},
		{"automatic", `,"mtu":0`, false},
		{"negative", `,"mtu":-1`, false},
		{"below-dual-stack-minimum", `,"mtu":1279`, false},
		{"minimum", `,"mtu":1280`, true},
		{"fits", `,"mtu":1420`, true},
		{"exceeds-path", `,"mtu":1450`, false},
		{"overflow", `,"mtu":65536`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := &appsv1.DaemonSet{}
			ds.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "install-cni", Env: []corev1.EnvVar{{Name: "CNI_NETWORK_CONFIG", Value: `{"plugins":[{"type":"calico"` + tc.field + `}]}`}}}}
			got := podMTU(context.Background(), nil, ds, 1420)
			if got.OK != tc.ok || !got.Required {
				t.Fatalf("MTU admission: %+v; want OK=%t", got, tc.ok)
			}
		})
	}
}

func TestPodMTURejectsInvalidTunnelBudget(t *testing.T) {
	for _, mtu := range []int{-1, 0, 1279, 65536} {
		t.Run(itoa(mtu), func(t *testing.T) {
			got := podMTU(context.Background(), nil, nil, mtu)
			if got.OK || !got.Required || !strings.Contains(got.Detail, "tunnel MTU") {
				t.Fatalf("invalid tunnel budget admitted: %+v", got)
			}
		})
	}
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func TestCalicoPreflightCannotQualifyAWSAllocations(t *testing.T) {
	c := observedCluster(t, nil)
	nodes := []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "aws", Labels: map[string]string{"cloud-provisioning.appmana.com/cni": "aws-vpc"}}}}
	got := masquerade(context.Background(), c, cni.Network{Name: cni.Calico, Encapsulation: cni.Native}, nodes)
	if got.OK || !got.Required {
		t.Fatal("Calico preflight claimed AWS source preservation")
	}
}

func TestPlatformTupleChecksCatchCalicoFelixMTUDrift(t *testing.T) {
	// With only CNI_NETWORK_CONFIG lowered to 1420 while FELIX_IPINIPMTU remains 1500,
	// tuple-aware Preconditions catches the CNI vs Felix MTU drift.
	c := observedCluster(t, func(ds *appsv1.DaemonSet) { setCNIMTU(ds, 1420) })
	results, err := Preconditions(context.Background(), c, Options{
		TunnelMTU: 1420,
		Tuple:     "k0s + calico,bgp + aws",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := byName(results)
	if !got["tuple"].OK || !got["calico-autodetection"].OK || !got["k0s-windows-egress"].OK {
		t.Fatalf("expected tuple/autodetection/egress to pass: %+v", got)
	}
	if got["calico-mtu"].OK || !strings.Contains(got["calico-mtu"].Detail, "disagrees with FELIX_IPINIPMTU=1500") {
		t.Fatalf("expected calico-mtu to fail on 1420 vs 1500 drift, got: %+v", got["calico-mtu"])
	}

	// Aligning FELIX_*MTU with CNI_NETWORK_CONFIG (1420) passes all checks.
	aligned := observedCluster(t, func(ds *appsv1.DaemonSet) {
		setCNIMTU(ds, 1420)
		for i := range ds.Spec.Template.Spec.Containers {
			for j := range ds.Spec.Template.Spec.Containers[i].Env {
				switch ds.Spec.Template.Spec.Containers[i].Env[j].Name {
				case "FELIX_IPINIPMTU", "FELIX_VXLANMTU", "FELIX_WIREGUARDMTU":
					ds.Spec.Template.Spec.Containers[i].Env[j].Value = "1420"
				}
			}
		}
	})
	results, err = Preconditions(context.Background(), aligned, Options{
		TunnelMTU: 1420,
		Tuple:     "k0s + calico,bgp + aws",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !Passed(results) {
		t.Fatalf("expected aligned tuple preconditions to pass: %+v", results)
	}
}
