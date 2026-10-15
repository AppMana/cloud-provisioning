package main

import (
	"net"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// Only explicit node addresses use host-route readiness and endpoint handling.
// Other permitted prefixes are pod allocations even when they contain one IP.
func isPeerNodeAddress(peer tunnel.PeerSpec, prefix net.IPNet) bool {
	for _, raw := range peer.AllRouteHosts() {
		host, err := parseHostRoute(raw)
		if err == nil && host.String() == prefix.String() {
			return true
		}
	}
	return false
}
