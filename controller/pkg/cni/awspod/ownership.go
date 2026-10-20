package awspod

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// ValidateNodeCNI rejects competing Calico BGP ownership before native AWS
// allocation publication. It never removes another CNI's configuration. The
// ownership label alone does not establish which CNI is actually installed;
// runtime CNI ownership remains a separate deployment requirement.
func ValidateNodeCNI(node *corev1.Node) error {
	if node == nil || node.Labels[NodeLabel] != NodeValue {
		return fmt.Errorf("AWS VPC CNI ownership label required")
	}
	for _, key := range []string{"projectcalico.org/IPv4Address", "projectcalico.org/IPv6Address"} {
		if node.Annotations[key] != "" {
			return fmt.Errorf("AWS VPC CNI node %s retains Calico BGP address annotation %s", node.Name, key)
		}
	}
	return nil
}
