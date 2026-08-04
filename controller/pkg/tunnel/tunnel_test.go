package tunnel

import (
	"strings"
	"testing"
)

func TestHostCIDR(t *testing.T) {
	cases := map[string]string{
		"10.100.0.2":        "10.100.0.2/32",
		"fd8f:cf26:522a::1": "fd8f:cf26:522a::1/128",
		"10.100.0.2/32":     "10.100.0.2/32", // already has a prefix, left alone
		"10.100.0.0/24":     "10.100.0.0/24", // not narrowed if already broader (rejection is the route parser's job)
	}
	for in, want := range cases {
		if got := HostCIDR(in); got != want {
			t.Errorf("HostCIDR(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitList(t *testing.T) {
	got := SplitList("10.244.0.0/16, ", "10.96.0.0/12,fd00::/108", "")
	want := []string{"10.244.0.0/16", "10.96.0.0/12", "fd00::/108"}
	if len(got) != len(want) {
		t.Fatalf("SplitList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SplitList[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestInterfaceName(t *testing.T) {
	name := InterfaceName("wg-dialer/wg-dialer-peer")
	if !strings.HasPrefix(name, "cldt") {
		t.Errorf("InterfaceName = %q, want cldt prefix", name)
	}
	if len(name) != 12 {
		t.Errorf("InterfaceName = %q (len %d), want 12 chars (under IFNAMSIZ 15)", name, len(name))
	}
	if name == "wg0" || strings.HasPrefix(name, "wg") {
		t.Errorf("InterfaceName = %q must never collide with the wgX namespace", name)
	}
	if InterfaceName("wg-dialer/wg-dialer-peer") != name {
		t.Error("InterfaceName is not deterministic")
	}
	if InterfaceName("other/mesh") == name {
		t.Error("InterfaceName does not vary with mesh identity")
	}
}

func TestAllRouteHosts_FoldsLegacyField(t *testing.T) {
	p := PeerSpec{RouteHosts: []string{"10.100.0.2", "10.101.0.4"}, RouteHost: "fd8f:cf26:522a::4"}
	got := p.AllRouteHosts()
	if len(got) != 3 {
		t.Fatalf("AllRouteHosts = %v, want 3 entries", got)
	}
}

// peerSecret builds the published state for a mesh of local nodes, the
// shape the mesh reconciler writes.
func peerSecret(nodes map[string][2]string, pods map[string]string) map[string][]byte {
	data := map[string][]byte{}
	for name, pair := range nodes {
		data[NodePublicKeyPrefix+name] = []byte(pair[0])
		data[NodeTunnelAddressPrefix+name] = []byte(pair[1])
	}
	for name, cidrs := range pods {
		data[NodePodCIDRsPrefix+name] = []byte(cidrs)
	}
	return data
}

// WireGuard's accept list is a trie with a single owner per prefix, so
// the same prefix on two peers belongs to whichever was configured
// last. Every peer must therefore carry only prefixes no other peer
// carries.
func TestRemotePeers_AllowedIPsArePairwiseDisjoint(t *testing.T) {
	data := peerSecret(
		map[string][2]string{
			"worker-1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"},
			"worker-2": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.2/24"},
			"worker-3": {"ccccccccccccccccccccccccccccccccccccccccccC=", "10.100.0.3/24"},
		},
		map[string]string{
			"worker-1": "10.244.1.0/26,10.244.4.0/26",
			"worker-2": "10.244.2.0/26",
			"worker-3": "10.244.3.0/26",
		},
	)
	peers, err := RemotePeers(data, "10.100.0.128", nil)
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}
	if len(peers) != 3 {
		t.Fatalf("got %d peers, want 3", len(peers))
	}
	owner := map[string]int{}
	for i, p := range peers {
		for _, cidr := range p.WGAllowedIPs {
			if prev, seen := owner[cidr]; seen {
				t.Errorf("%s is permitted on peer %d and peer %d; the trie gives it to one of them", cidr, prev, i)
			}
			owner[cidr] = i
		}
	}
}

// A peer carries the blocks of its own node. Carrying another node's
// block would take that node's traffic and send it to the wrong peer.
func TestRemotePeers_EachPeerCarriesOnlyItsOwnBlocks(t *testing.T) {
	data := peerSecret(
		map[string][2]string{
			"worker-1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"},
			"worker-2": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.2/24"},
		},
		map[string]string{
			"worker-1": "10.244.1.0/26",
			"worker-2": "10.244.2.0/26",
		},
	)
	peers, err := RemotePeers(data, "10.100.0.128", nil)
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}
	for _, p := range peers {
		var own, foreign string
		switch p.PublicKey {
		case "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=":
			own, foreign = "10.244.1.0/26", "10.244.2.0/26"
		case "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=":
			own, foreign = "10.244.2.0/26", "10.244.1.0/26"
		default:
			t.Fatalf("unexpected peer %s", p.PublicKey)
		}
		var hasOwn, hasForeign bool
		for _, cidr := range p.WGAllowedIPs {
			if cidr == own {
				hasOwn = true
			}
			if cidr == foreign {
				hasForeign = true
			}
		}
		if !hasOwn {
			t.Errorf("peer %s does not carry its own block %s: %v", p.PublicKey, own, p.WGAllowedIPs)
		}
		if hasForeign {
			t.Errorf("peer %s carries another node's block %s: %v", p.PublicKey, foreign, p.WGAllowedIPs)
		}
	}
}

// An encapsulated network publishes no pod blocks, and the accept list
// must then be node addresses alone.
func TestRemotePeers_EncapsulatedCarriesHostsOnly(t *testing.T) {
	data := peerSecret(
		map[string][2]string{"worker-1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"}},
		nil,
	)
	data[NodeAddressesPrefix+"worker-1"] = []byte("10.0.0.11")
	peers, err := RemotePeers(data, "10.100.0.128", nil)
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}
	for _, cidr := range peers[0].WGAllowedIPs {
		if !strings.HasSuffix(cidr, "/32") && !strings.HasSuffix(cidr, "/128") {
			t.Errorf("%s is not a host address, but the network encapsulates", cidr)
		}
	}
}

// Two remotes on different clouds share no network, so the only thing
// that makes them mutually reachable is an edge between them. Each
// remote's own view of the mesh has to carry the other as a peer, with
// that other's addresses and blocks and with an endpoint to dial, or
// the two are joined to the same cluster and invisible to each other.
func TestRemotePeers_RemotesReachEachOther(t *testing.T) {
	data := peerSecret(
		map[string][2]string{"worker-1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"}},
		map[string]string{"worker-1": "10.244.1.0/26"},
	)
	// Two machines, as the mesh reconciler publishes them.
	for _, m := range []struct{ name, key, endpoint, addr, block string }{
		{"cloud-1", "ddddddddddddddddddddddddddddddddddddddddddD=", "203.0.113.10:51820", "10.100.0.128", "10.244.123.128/26"},
		{"cloud-2", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeE=", "198.51.100.20:51820", "10.100.0.129", "10.244.231.192/26"},
	} {
		data[PeerPublicKeyPrefix+m.name] = []byte(m.key)
		data[PeerEndpointPrefix+m.name] = []byte(m.endpoint)
		data[PeerRouteHostsPrefix+m.name] = []byte(m.addr)
		data[PeerAllowedIPsPrefix+m.name] = []byte(HostCIDR(m.addr) + "," + m.block)
	}

	// From cloud-1's side.
	peers, err := RemotePeers(data, "10.100.0.128", nil)
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}
	var other *PeerSpec
	for i := range peers {
		if peers[i].PublicKey == "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeE=" {
			other = &peers[i]
		}
		if peers[i].PublicKey == "ddddddddddddddddddddddddddddddddddddddddddD=" {
			t.Error("a remote carries itself as a peer")
		}
	}
	if other == nil {
		t.Fatal("the other remote is absent, so the two cannot reach each other at all")
	}
	// An endpoint, because neither is behind the other's NAT: each has
	// to be able to dial the other directly.
	if other.Endpoint != "198.51.100.20:51820" {
		t.Errorf("the other remote has no endpoint to dial: %q", other.Endpoint)
	}
	var hasAddr, hasBlock bool
	for _, cidr := range other.WGAllowedIPs {
		switch cidr {
		case "10.100.0.129/32":
			hasAddr = true
		case "10.244.231.192/26":
			hasBlock = true
		}
	}
	if !hasAddr {
		t.Errorf("the other remote's address is not permitted: %v", other.WGAllowedIPs)
	}
	if !hasBlock {
		t.Errorf("the other remote's pods are not permitted: %v", other.WGAllowedIPs)
	}
	if !containsHost(other.AllRouteHosts(), "10.100.0.129") {
		t.Errorf("no route to the other remote: %v", other.AllRouteHosts())
	}
}

// A remote reaches a site node that has no tunnel by relaying through
// one that does. The route for that node's pods comes from the network,
// but WireGuard checks its accept list on the way out as well as in, so
// a block that is routed into the tunnel and permitted by nothing is
// dropped by the sender. Measured: the remote held
// "10.244.168.64/26 via ... dev cldt..." and an accept list without it,
// and every pair involving a node with no tunnel failed in both
// directions.
func TestRemotePeers_PermitsTheSiteNodesWithNoTunnel(t *testing.T) {
	data := peerSecret(
		map[string][2]string{
			"endpoint-1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"},
			"endpoint-2": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.2/24"},
		},
		map[string]string{"endpoint-1": "10.244.63.192/26", "endpoint-2": "10.244.1.0/26"},
	)
	// Two nodes at the site with no tunnel of their own.
	data[SiteAddressesPrefix+"control-plane"] = []byte("172.21.0.18")
	data[SitePodCIDRsPrefix+"control-plane"] = []byte("10.244.168.64/26")
	data[SiteAddressesPrefix+"worker-2"] = []byte("172.21.0.17")
	data[SitePodCIDRsPrefix+"worker-2"] = []byte("10.244.231.192/26")

	peers, err := RemotePeers(data, "10.100.0.128", nil)
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}

	// Every site prefix, on exactly one peer: the accept list has one
	// owner per prefix, so the same block on two peers belongs to
	// whichever was written last.
	for _, want := range []string{"172.21.0.18/32", "10.244.168.64/26", "172.21.0.17/32", "10.244.231.192/26"} {
		owners := 0
		for _, p := range peers {
			for _, cidr := range p.WGAllowedIPs {
				if cidr == want {
					owners++
				}
			}
		}
		if owners != 1 {
			t.Errorf("%s is permitted on %d peers, want exactly 1", want, owners)
		}
	}

	// A pod block is permitted but never routed: the route comes from
	// the network over the session the host routes make possible.
	for _, p := range peers {
		for _, host := range p.AllRouteHosts() {
			if strings.Contains(host, "/") {
				t.Errorf("route host %q is a prefix, not a host", host)
			}
		}
	}
}
