package main

import (
	"net"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestReceiveNeighborEndpointPreservesAuthenticatedRoaming(t *testing.T) {
	now := time.Now()
	configured := &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 51820}
	roamed := &net.UDPAddr{IP: net.ParseIP("198.51.100.20"), Port: 25758}
	for _, tc := range []struct {
		name      string
		endpoint  *net.UDPAddr
		handshake time.Time
		preserve  bool
	}{
		{"authenticated NAT mapping", roamed, now.Add(-time.Second), true},
		{"freshness boundary", roamed, now.Add(-receiveHandshakeMaxAge), true},
		{"stale mapping retries configured address", roamed, now.Add(-receiveHandshakeMaxAge - time.Nanosecond), false},
		{"unauthenticated mapping", roamed, time.Time{}, false},
		{"future handshake", roamed, now.Add(time.Second), false},
		{"missing endpoint", nil, now, false},
		{"new peer", nil, time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := receiveNeighborEndpoint(configured, wgtypes.Peer{Endpoint: tc.endpoint, LastHandshakeTime: tc.handshake}, now)
			if tc.preserve && got != nil {
				t.Fatal("authenticated roaming endpoint would be overwritten")
			}
			if !tc.preserve && got != configured {
				t.Fatal("configured fallback was suppressed without fresh authentication")
			}
		})
	}
}
