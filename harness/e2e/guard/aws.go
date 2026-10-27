package guard

import (
	"fmt"
	"slices"
)

// productEC2 are EC2 operations on what the product owns at run time: pod
// addresses, return routes, gateway interfaces and ingress for its tunnels.
// The operator's own network setup (a security group's rule admitting
// itself) is done once, by the setup scripts named in operatorEC2.
var productEC2 = []string{
	"create-route", "delete-route", "replace-route",
	"modify-network-interface-attribute",
	"assign-private-ip-addresses", "unassign-private-ip-addresses", "assign-ipv6-addresses", "unassign-ipv6-addresses",
	"create-network-interface", "delete-network-interface", "attach-network-interface", "detach-network-interface",
	"authorize-security-group-ingress", "revoke-security-group-ingress",
}

// operatorEC2 are the setup scripts that make the operator's network
// changes a deployment's documentation asks for.
var operatorEC2 = map[string][]string{
	"authorize-security-group-ingress": {"aws/bootstrap.py", "aws/native_access.py"},
	// The new VPC's default route to its internet gateway.
	"create-route": {"aws/bootstrap.py"},
}

// AWSStandIns are product EC2 operations the harness still performs, each
// with the open tracker item that removes it.
var AWSStandIns = map[string]string{}

// AWS allows or refuses an AWS operation the harness makes at run time.
func AWS(service, operation string) error {
	if service != "ec2" || !slices.Contains(productEC2, operation) {
		return nil
	}
	if _, ok := AWSStandIns[operation]; ok {
		return nil
	}
	return fmt.Errorf("%w: ec2 %s changes what the product owns; if a real deployment needs it, the product must do it", ErrRefused, operation)
}
