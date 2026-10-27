package platform

import corev1 "k8s.io/api/core/v1"

// ControlPlaneLabel is the conventional control-plane role label and taint key.
const ControlPlaneLabel = "node-role.kubernetes.io/control-plane"

// ControlPlaneMarks are the labels each distribution puts on its control
// planes, keyed by label with the value it must carry ("" = any). The
// conventional role label is not universal: k0s publishes only its own
// role label and MicroK8s only its own, so a site on either had its
// controllers chosen as tunnel endpoints.
var ControlPlaneMarks = map[string]string{
	ControlPlaneLabel:                          "",
	"node-role.kubernetes.io/master":           "",
	"node.k0sproject.io/role":                  "control-plane",
	"node.kubernetes.io/microk8s-controlplane": "",
}

// IsControlPlane reports whether a site node is a control plane, by any
// distribution's label or by the conventional control-plane taint.
func IsControlPlane(node *corev1.Node) bool {
	for key, want := range ControlPlaneMarks {
		if value, ok := node.Labels[key]; ok && (want == "" || value == want) {
			return true
		}
	}
	for _, taint := range node.Spec.Taints {
		if taint.Key == ControlPlaneLabel || taint.Key == "node-role.kubernetes.io/master" {
			return true
		}
	}
	return false
}
