package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"strings"
)

// GuestExec must bind execution to the exact instance and interface identity.
// Implementations may use SSM or a single-NIC VM's out-of-band guest agent.
type GuestExec interface {
	Read(context.Context, InterfaceTarget, []string) ([]byte, error)
}

// LinuxGatewayGuest observes forwarding already managed by k0s and the dialer.
// It adds no sysctls, routes or NAT rules, so it has no guest mutation to undo.
// Route lookups model forwarded traffic using the actual source and ingress NIC.
type LinuxGatewayGuest struct {
	Exec                             GuestExec
	NativeInterface, TunnelInterface string
}

func (g LinuxGatewayGuest) Ensure(ctx context.Context, r attachment.Record, b ForwardingBinding) (bool, error) {
	if g.Exec == nil || g.NativeInterface == "" || g.TunnelInterface == "" || g.NativeInterface == g.TunnelInterface || len(r.Plan.ReturnHosts) == 0 {
		return false, fmt.Errorf("guest executor, distinct interfaces and return hosts required")
	}
	if b.Lease != r.LeaseID() || b.Gateway.InterfaceID != r.Plan.Gateway.InterfaceID {
		return false, fmt.Errorf("guest binding mismatch")
	}
	raw, err := g.Exec.Read(ctx, b.Gateway, []string{"cat", "/proc/sys/net/ipv4/ip_forward"})
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(raw)) != "1" {
		return false, nil
	}
	lookup := func(destination, source, input, output string) (bool, error) {
		raw, err := g.Exec.Read(ctx, b.Gateway, []string{"ip", "-j", "route", "get", destination, "from", source, "iif", input})
		if err != nil {
			return false, err
		}
		var routes []struct{ Dst, Dev, Type string }
		if err = json.Unmarshal(raw, &routes); err != nil {
			return false, err
		}
		return len(routes) == 1 && routes[0].Dst == destination && routes[0].Dev == output && (routes[0].Type == "" || routes[0].Type == "unicast"), nil
	}
	for _, host := range r.Plan.ReturnHosts {
		if ok, err := lookup(r.Plan.Worker.Address.String(), host.Addr().String(), g.TunnelInterface, g.NativeInterface); err != nil || !ok {
			return false, err
		}
		if ok, err := lookup(host.Addr().String(), r.Plan.Worker.Address.String(), g.NativeInterface, g.TunnelInterface); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}
func (g LinuxGatewayGuest) Release(_ context.Context, r attachment.Record, b ForwardingBinding) (bool, error) {
	if b.Lease != r.LeaseID() {
		return false, fmt.Errorf("guest release lease mismatch")
	}
	return true, nil
}
