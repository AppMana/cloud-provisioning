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
