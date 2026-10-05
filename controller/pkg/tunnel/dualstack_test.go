package tunnel

import (
	"net"
	"slices"
	"testing"
)

// A tunnel's IPv6 address is its IPv4 allocation carried in the low 32
// bits of the mesh's IPv6 prefix. One allocator, one reservation and one
// retirement record therefore cover both families: an IPv4 address that
// is never handed to another node makes its IPv6 twin unique as well.
func TestTunnelAddress6CarriesTheIPv4Allocation(t *testing.T) {
	for _, tc := range []struct{ prefix, addr4, want string }{
		{"fd00:10:100::/96", "10.100.0.1/24", "fd00:10:100::a64:1/96"},
		{"fd00:10:100::/96", "10.100.0.128", "fd00:10:100::a64:80/96"},
		// Host bits in the configured prefix are not part of it.
		{"fd00:10:100::ffff:ffff/96", "10.100.0.2/24", "fd00:10:100::a64:2/96"},
		{"fd00:10::/64", "10.100.0.3/24", "fd00:10::a64:3/64"},
	} {
		got, err := TunnelAddress6(tc.prefix, tc.addr4)
		if err != nil {
			t.Fatalf("TunnelAddress6(%q, %q): %v", tc.prefix, tc.addr4, err)
		}
		if got != tc.want {
			t.Errorf("TunnelAddress6(%q, %q) = %q, want %q", tc.prefix, tc.addr4, got, tc.want)
		}
	}
	for _, tc := range []struct{ prefix, addr4 string }{
		// Too long to hold 32 bits of host.
		{"fd00:10:100::/112", "10.100.0.1/24"},
		{"10.0.0.0/8", "10.100.0.1/24"},
		{"fd00:10:100::/96", "fd00::1"},
		{"fd00:10:100::/96", ""},
		{"", "10.100.0.1/24"},
	} {
		if got, err := TunnelAddress6(tc.prefix, tc.addr4); err == nil {
			t.Errorf("TunnelAddress6(%q, %q) = %q, want an error", tc.prefix, tc.addr4, got)
		}
	}
}

// A remote reaches a dual-stack site endpoint at both of its tunnel
// addresses. The IPv6 one is the source the endpoint's own IPv6 traffic
// leaves the tunnel with, so it has to be both permitted and routed, and
// it belongs to that endpoint's entry alone.
func TestRemotePeers_CarryTheEndpointsIPv6TunnelAddress(t *testing.T) {
	data := map[string][]byte{
		NodePublicKeyPrefix + "w1":      []byte("W1KEY"),
		NodeTunnelAddressPrefix + "w1":  []byte("10.100.0.1/24"),
		NodeTunnelAddress6Prefix + "w1": []byte("fd00:10:100::a64:1/96"),
		NodeAddressesPrefix + "w1":      []byte("10.101.0.2,fd8f:cf26:522a::2"),
		NodePublicKeyPrefix + "w2":      []byte("W2KEY"),
		NodeTunnelAddressPrefix + "w2":  []byte("10.100.0.2/24"),
		NodeTunnelAddress6Prefix + "w2": []byte("fd00:10:100::a64:2/96"),
		NodeAddressesPrefix + "w2":      []byte("10.101.0.3,fd8f:cf26:522a::3"),
	}
	peers, err := RemotePeers(data, "10.100.0.128", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"W1KEY": "fd00:10:100::a64:1", "W2KEY": "fd00:10:100::a64:2"}
	for _, p := range peers {
		addr := want[p.PublicKey]
		if !slices.Contains(p.WGAllowedIPs, addr+"/128") {
			t.Errorf("%s does not permit its IPv6 tunnel address: %v", p.PublicKey, p.WGAllowedIPs)
		}
		if !slices.Contains(p.RouteHosts, addr) {
			t.Errorf("%s does not route its IPv6 tunnel address: %v", p.PublicKey, p.RouteHosts)
		}
		for other, otherAddr := range want {
			if other != p.PublicKey && slices.Contains(p.WGAllowedIPs, otherAddr+"/128") {
				t.Errorf("%s permits %s's IPv6 tunnel address", p.PublicKey, other)
			}
		}
	}
}

// A site node with no tunnel forwards a remote's prefixes to the relay,
// and a route's gateway must be of the destination's own family: Linux
// refuses an IPv6 route through an IPv4 gateway, so a single next hop
// fails every IPv6 destination and, in the same pass, every route after
// it. The relay's IPv6 next hop is the address it published for that
// purpose, which on a site whose node addresses sit on a dummy device is
// not one of them.
func TestSiteTransit_HasANextHopPerFamily(t *testing.T) {
	data := map[string][]byte{
		NodePublicKeyPrefix + "w1":       []byte("W1KEY"),
		NodeTunnelAddressPrefix + "w1":   []byte("10.100.0.1/24"),
		NodeAddressesPrefix + "w1":       []byte("10.101.0.2,fd8f:cf26:522a::2"),
		NodeTransitAddress6Prefix + "w1": []byte("fd8f:cf26:522a:0:64e3:4ca4:cc0:665c"),

		PeerPublicKeyPrefix + "remote1":  []byte("R1KEY"),
		PeerRouteHostsPrefix + "remote1": []byte("10.100.0.128,fd00:10:100::a64:80"),
		PeerAllowedIPsPrefix + "remote1": []byte("10.100.0.128/32,fd00:10:100::a64:80/128,10.101.200.0/26,fd8f:cf26:522a:128:abcd::/122"),
	}
	transit, err := SiteTransit(data, nil)
	if err != nil || transit == nil {
		t.Fatalf("SiteTransit = %+v, %v", transit, err)
	}
	if transit.Via != "10.101.0.2" {
		t.Errorf("Via = %q, want the relay's first node address", transit.Via)
	}
	for dst, want := range map[string]string{
		"10.100.0.128":              "10.101.0.2",
		"10.101.200.0":              "10.101.0.2",
		"fd00:10:100::a64:80":       "fd8f:cf26:522a:0:64e3:4ca4:cc0:665c",
		"fd8f:cf26:522a:128:abcd::": "fd8f:cf26:522a:0:64e3:4ca4:cc0:665c",
	} {
		got := transit.Gateway(net.ParseIP(dst))
		if got == nil || got.String() != want {
			t.Errorf("Gateway(%s) = %v, want %s", dst, got, want)
		}
	}

	// Without a published transit address the relay's own IPv6 node
	// address is the next hop.
	delete(data, NodeTransitAddress6Prefix+"w1")
	transit, err = SiteTransit(data, nil)
	if err != nil || transit == nil {
		t.Fatalf("SiteTransit = %+v, %v", transit, err)
	}
	if got := transit.Gateway(net.ParseIP("fd00:10:100::a64:80")); got == nil || got.String() != "fd8f:cf26:522a::2" {
		t.Errorf("Gateway without a transit address = %v, want the relay's IPv6 node address", got)
	}

	// A single-stack relay has no IPv6 next hop, and none is invented.
	data[NodeAddressesPrefix+"w1"] = []byte("10.101.0.2")
	transit, err = SiteTransit(data, nil)
	if err != nil || transit == nil {
		t.Fatalf("SiteTransit = %+v, %v", transit, err)
	}
	if got := transit.Gateway(net.ParseIP("fd00:10:100::a64:80")); got != nil {
		t.Errorf("Gateway on a single-stack relay = %v, want none", got)
	}
}

// Every site node acknowledges the transit it applied by hashing it, and
// the controller compares those hashes. A single-stack mesh must hash
// exactly as it did before the second family existed, or upgrading the
// controller would make every receipt in an existing mesh stale at once.
func TestSiteTransitHash_SingleStackIsUnchanged(t *testing.T) {
	data := map[string][]byte{
		NodePublicKeyPrefix + "w1":       []byte("W1KEY"),
		NodeTunnelAddressPrefix + "w1":   []byte("10.100.0.17/24"),
		NodeAddressesPrefix + "w1":       []byte("10.10.0.11"),
		PeerPublicKeyPrefix + "remote1":  []byte("R1KEY"),
		PeerRouteHostsPrefix + "remote1": []byte("10.100.0.128"),
		PeerAllowedIPsPrefix + "remote1": []byte("10.100.0.128/32,10.244.159.0/26"),
	}
	transit, err := SiteTransit(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := SiteTransitHash(data, transit)
	if err != nil {
		t.Fatal(err)
	}
	// Recorded from the single-family implementation.
	const want = "d22704d093ab9c733442450a244f95ea85c8b687f67fb1c7714e774ac393f96d"
	if got != want {
		t.Errorf("single-stack transit hash = %s, want %s", got, want)
	}
}
