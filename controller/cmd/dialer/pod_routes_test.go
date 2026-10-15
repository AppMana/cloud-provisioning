package main

import (
	"net"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

func TestPodRoutesUseOwnershipRatherThanPrefixWidth(t *testing.T) {
	peer := tunnel.PeerSpec{RouteHosts: []string{"10.100.0.128", "172.29.0.104/32", "fd00::128"}}
	for _, tc := range []struct {
		prefix string
		node   bool
	}{
		{"10.100.0.128/32", true}, {"172.29.0.104/32", true}, {"fd00::128/128", true},
		{"172.29.0.90/32", false}, {"fd00::129/128", false},
		{"10.101.128.0/26", false}, {"2001:db8:1::/80", false},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			_, prefix, err := net.ParseCIDR(tc.prefix)
			if err != nil {
				t.Fatal(err)
			}
			if got := isPeerNodeAddress(peer, *prefix); got != tc.node {
				t.Fatalf("node address = %v, want %v", got, tc.node)
			}
		})
	}
}
