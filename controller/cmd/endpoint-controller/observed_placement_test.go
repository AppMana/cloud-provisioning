package main

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"testing"
)

// Labels observed on the real k0s/Calico control plane. Older synthetic
// fixtures named a node cp but omitted its role label, hiding this distinction.
func TestObservedControlPlaneRoleRequiresExplicitOptIn(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp", Labels: map[string]string{
		"kubernetes.io/hostname": "cp", "kubernetes.io/os": "linux",
		"node-role.kubernetes.io/control-plane": "true", "node.k0sproject.io/role": "control-plane",
	}}}
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"kubernetes.io/hostname=cp", false},
		{"node-role.kubernetes.io/control-plane,kubernetes.io/hostname=cp", true},
	} {
		selector, err := labels.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		r := &meshReconciler{tunnelEndpointsRaw: tc.raw, tunnelEndpointSelector: selector}
		if got := r.isTunnelEndpoint(node); got != tc.want {
			t.Fatalf("selector %q: selected=%v", tc.raw, got)
		}
	}
}
