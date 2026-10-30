package main

import (
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"testing"
)

// The labels and taints each distribution actually gives a control plane,
// with nothing added by an operator. k0s publishes only its own role label
// (and, without --no-taints, the conventional taint); MicroK8s only its own
// label. An earlier fixture carried node-role.kubernetes.io/control-plane on
// a k0s controller because the lab added it, which hid that a stock k0s
// controller was chosen as a tunnel endpoint.
var stockControlPlanes = map[string]*corev1.Node{
	"k0s controller+worker --no-taints": {ObjectMeta: metav1.ObjectMeta{Name: "cp", Labels: map[string]string{
		"kubernetes.io/hostname": "cp", "kubernetes.io/os": "linux", "node.k0sproject.io/role": "control-plane",
	}}},
	"k0s controller+worker": {ObjectMeta: metav1.ObjectMeta{Name: "cp", Labels: map[string]string{
		"kubernetes.io/hostname": "cp", "kubernetes.io/os": "linux", "node.k0sproject.io/role": "control-plane",
	}}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}}},
	"microk8s": {ObjectMeta: metav1.ObjectMeta{Name: "cp", Labels: map[string]string{
		"kubernetes.io/hostname": "cp", "kubernetes.io/os": "linux", "node.kubernetes.io/microk8s-controlplane": "microk8s-controlplane",
	}}},
	"kubeadm": {ObjectMeta: metav1.ObjectMeta{Name: "cp", Labels: map[string]string{
		"kubernetes.io/hostname": "cp", "kubernetes.io/os": "linux", "node-role.kubernetes.io/control-plane": "",
	}}},
}

func TestStockControlPlanesAreRecognised(t *testing.T) {
	for name, node := range stockControlPlanes {
		if !platform.IsControlPlane(node) {
			t.Errorf("%s: not recognised as a control plane", name)
		}
	}
	worker := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w", Labels: map[string]string{
		"kubernetes.io/os": "linux", "node.k0sproject.io/role": "worker",
	}}}
	if platform.IsControlPlane(worker) {
		t.Error("a k0s worker was taken for a control plane")
	}
}

// A control plane terminates tunnels only when the selector names it, in
// whichever label its distribution uses.
func TestStockControlPlanesRequireExplicitOptIn(t *testing.T) {
	for name, node := range stockControlPlanes {
		for _, tc := range []struct {
			raw  string
			want bool
		}{
			{"kubernetes.io/hostname=cp", false},
			{"node-role.kubernetes.io/control-plane,kubernetes.io/hostname=cp", name == "kubeadm"},
			{"node.k0sproject.io/role=control-plane,kubernetes.io/hostname=cp", name == "k0s controller+worker" || name == "k0s controller+worker --no-taints"},
			{"node.kubernetes.io/microk8s-controlplane,kubernetes.io/hostname=cp", name == "microk8s"},
			{"all", true},
		} {
			selector, err := labels.Parse(tc.raw)
			if err != nil && tc.raw != "all" {
				t.Fatal(err)
			}
			r := &meshReconciler{tunnelEndpointsRaw: tc.raw, tunnelEndpointSelector: selector}
			if got := r.isTunnelEndpoint(node); got != tc.want {
				t.Errorf("%s, selector %q: selected=%v", name, tc.raw, got)
			}
		}
	}
}

// In a mixed-architecture site where cp is linux/amd64 with
// node.k0sproject.io/role=control-plane and w1/w2 are linux/arm64 with
// node.k0sproject.io/role=worker, alongside an arm64 t4g.small cloud worker,
// site dialer affinity must schedule across both site architectures while
// excluding the cloud worker, and endpoint failover from the amd64 controller
// to the arm64 workers must preserve node identities.
func TestMixedArchitectureSiteDialerAndEndpointFailover(t *testing.T) {
	cp := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cp",
			Labels: map[string]string{
				"kubernetes.io/hostname":  "cp",
				"kubernetes.io/os":        "linux",
				"kubernetes.io/arch":      "amd64",
				"node.k0sproject.io/role": "control-plane",
			},
		},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "amd64"},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.101.0.1"},
				{Type: corev1.NodeInternalIP, Address: "fd8f:cf26:522a::1"},
			},
		},
	}
	w1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "w1",
			Labels: map[string]string{
				"kubernetes.io/hostname":  "w1",
				"kubernetes.io/os":        "linux",
				"kubernetes.io/arch":      "arm64",
				"node.k0sproject.io/role": "worker",
			},
		},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "arm64"},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.101.0.2"},
				{Type: corev1.NodeInternalIP, Address: "fd8f:cf26:522a::2"},
			},
		},
	}
	w2 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "w2",
			Labels: map[string]string{
				"kubernetes.io/hostname":  "w2",
				"kubernetes.io/os":        "linux",
				"kubernetes.io/arch":      "arm64",
				"node.k0sproject.io/role": "worker",
			},
		},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "arm64"},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.101.0.3"},
				{Type: corev1.NodeInternalIP, Address: "fd8f:cf26:522a::3"},
			},
		},
	}
	cloudWorker := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ip-10-42-1-20.ap-east-1.compute.internal",
			Labels: map[string]string{
				"kubernetes.io/os":   "linux",
				"kubernetes.io/arch": "arm64",
				cloudWorkerRoleLabel: cloudWorkerRoleValue,
			},
		},
	}

	r, c := dualStackReconciler(t, "fd00:10:100::/96", cp, w1, w2, cloudWorker)
	cpSel, err := labels.Parse("node.k0sproject.io/role=control-plane")
	if err != nil {
		t.Fatal(err)
	}
	r.tunnelEndpointsRaw = "node.k0sproject.io/role=control-plane"
	r.tunnelEndpointSelector = cpSel
	affinity := r.siteDialerNodeAffinity()
	for _, n := range []*corev1.Node{cp, w1, w2} {
		if !matchesNode(affinity, n) {
			t.Fatalf("site dialer affinity rejected %s (%s)", n.Name, n.Labels["kubernetes.io/arch"])
		}
	}
	if matchesNode(affinity, cloudWorker) {
		t.Fatal("site dialer affinity matched cloud-worker")
	}

	// Phase 1: tunnel terminates on cp (amd64 control-plane); arm64 workers are site nodes.
	if !r.isTunnelEndpoint(cp) || r.isTunnelEndpoint(w1) || r.isTunnelEndpoint(w2) {
		t.Fatal("unexpected initial endpoint selection")
	}
	if _, err := r.reconcileTunnelEndpoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	data := meshData(t, c)
	if got := string(data["node-addresses-cp"]); got != "10.101.0.1,fd8f:cf26:522a::1" {
		t.Fatalf("cp addresses = %q", got)
	}
	if got := string(data["site-addresses-w1"]); got != "10.101.0.2,fd8f:cf26:522a::2" {
		t.Fatalf("w1 site addresses = %q", got)
	}

	// Phase 2: endpoint failover to the arm64 workers (node.k0sproject.io/role=worker).
	workerSel, err := labels.Parse("node.k0sproject.io/role=worker")
	if err != nil {
		t.Fatal(err)
	}
	r.tunnelEndpointsRaw = "node.k0sproject.io/role=worker"
	r.tunnelEndpointSelector = workerSel
	if r.isTunnelEndpoint(cp) || !r.isTunnelEndpoint(w1) || !r.isTunnelEndpoint(w2) {
		t.Fatal("unexpected failover endpoint selection")
	}
	if _, err := r.reconcileTunnelEndpoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	data = meshData(t, c)
	for _, name := range []string{"w1", "w2"} {
		if len(data["node-tunnel-address-"+name]) == 0 || len(data["node-tunnel-address6-"+name]) == 0 {
			t.Fatalf("%s did not receive dual-stack tunnel allocations on failover", name)
		}
	}
}
