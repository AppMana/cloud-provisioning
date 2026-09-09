package main

import (
	"net"
	"strings"
	"testing"
)

func TestTunnelSourceRoutesRejectUnsafePlansBeforeKernelAccess(t *testing.T) {
	_, host, _ := net.ParseCIDR("10.100.0.160/32")
	_, block, _ := net.ParseCIDR("10.100.0.0/24")
	_, v6, _ := net.ParseCIDR("fd20::160/128")
	for _, tc := range []struct {
		name, address string
		want          string
		table         int
		host          net.IPNet
	}{
		{"reserved rule priority", "10.100.0.1/24", "cannot reserve", 1, *host},
		{"overflowing companion table", "10.100.0.1/24", "cannot reserve", 2147483647, *host},
		{"invalid local source", "invalid", "parsing tunnel source", 517, *host},
		{"broad destination", "10.100.0.1/24", "same-family single host", 517, *block},
		{"different address family", "10.100.0.1/24", "same-family single host", 517, *v6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := reconcileTunnelSourceRoutes(config{iface: "must-not-be-used", routeTable: tc.table}, tc.address, []net.IPNet{tc.host}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want validation error %q before kernel access, got %v", tc.want, err)
			}
		})
	}
}
