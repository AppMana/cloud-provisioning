package guard

import (
	"errors"
	"testing"
)

func TestProductEC2ChangesAreRefused(t *testing.T) {
	for _, op := range []string{"modify-network-interface-attribute", "assign-private-ip-addresses", "authorize-security-group-ingress", "create-route"} {
		if err := AWS("ec2", op); !errors.Is(err, ErrRefused) {
			t.Errorf("ec2 %s: %v", op, err)
		}
	}
	for _, call := range [][2]string{{"ec2", "describe-instances"}, {"ssm", "send-command"}} {
		if err := AWS(call[0], call[1]); err != nil {
			t.Errorf("%s %s: %v", call[0], call[1], err)
		}
	}
}
