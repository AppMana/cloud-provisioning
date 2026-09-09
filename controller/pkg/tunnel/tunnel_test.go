package tunnel

import (
	"strings"
	"testing"
)

// The relay-egress decision is a content question, not a freshness
// question. A site node being relayed must not send a remote its
// traffic through the relay until that remote's APPLIED list actually
// carries the node's addresses on the relay: an applied hash that
// matches a list which still routes the node directly means the
// remote's accept list still owns those sources on the direct entry,
// and everything arriving via the relay is dropped by cryptokey
// routing. Measured: a placement shrink, cp3 switching to the relay
// on a fresh-but-direct acknowledgment, and 78 seconds of its return
// traffic dying inside remote2's trie.
func TestDocRelaysNode(t *testing.T) {
	direct := PeerListDoc{Peers: []PeerSpec{
		{PublicKey: "CP3", WGAllowedIPs: []string{"10.10.0.14/32", "10.244.2.0/24"}},
		{PublicKey: "W1", WGAllowedIPs: []string{"10.10.0.11/32"}},
	}}
	if DocRelaysNode(direct, "CP3", []string{"10.10.0.14"}) {
		t.Error("a list that still routes the node directly was taken as relaying it")
	}

	relayed := PeerListDoc{Peers: []PeerSpec{
		{PublicKey: "W1", WGAllowedIPs: []string{"10.10.0.11/32", "10.10.0.14/32", "10.244.2.0/24"},
			Transit: []string{"10.244.2.0/24"}, TransitHosts: []string{"10.10.0.14"}},
	}}
	if !DocRelaysNode(relayed, "CP3", []string{"10.10.0.14"}) {
		t.Error("a list that carries the node's addresses on the relay's transit was not taken as relaying it")
	}

	// Present on both: the direct entry still owns the sources (one
	// owner per prefix, and cryptokey routing gives a prefix to
	// whichever entry was written last), so this is not safely
	// relayed.
	both := PeerListDoc{Peers: []PeerSpec{
		{PublicKey: "CP3", WGAllowedIPs: []string{"10.10.0.14/32"}},
		{PublicKey: "W1", WGAllowedIPs: []string{"10.10.0.11/32"}, TransitHosts: []string{"10.10.0.14"}},
	}}
	if DocRelaysNode(both, "CP3", []string{"10.10.0.14"}) {
		t.Error("a list that still carries the direct entry was taken as relaying")
	}

	// No transit anywhere and no direct entry either: the node is
	// simply absent. Not relayed: sending via the relay would still
	// be dropped.
	absent := PeerListDoc{Peers: []PeerSpec{
		{PublicKey: "W1", WGAllowedIPs: []string{"10.10.0.11/32"}},
	}}
	if DocRelaysNode(absent, "CP3", []string{"10.10.0.14"}) {
		t.Error("a list with no trace of the node was taken as relaying it")
	}
}

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

// owners counts the peers permitting a prefix. One is the only correct
// answer: zero is a prefix a remote cannot reach at all, two is a prefix
// the accept list resolves to whichever peer was configured last.
func owners(peers []PeerSpec, prefix string) int {
	n := 0
	for _, p := range peers {
		for _, cidr := range p.WGAllowedIPs {
			if cidr == prefix {
				n++
			}
		}
	}
	return n
}

// A node returning to the selector is published twice for as long as it
// takes its own dialer to put a key in the Secret: the operator has
// already written its addresses under node-*, and the site-* entries
// that relayed to it while it had no tunnel are still there. Whichever
// of the two the render prefers, it must prefer exactly one.
func TestRemotePeers_AReturningEndpointOwnsItsPrefixesOnce(t *testing.T) {
	data := peerSecret(
		map[string][2]string{
			"w1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.17/24"},
			"cp": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.24/24"},
		},
		map[string]string{"w1": "10.244.190.64/26", "cp": "10.244.242.64/26"},
	)
	data[NodeAddressesPrefix+"w1"] = []byte("10.10.0.11")
	data[NodeAddressesPrefix+"cp"] = []byte("10.10.0.10")
	// cp was relayed through w1 until a moment ago, and the entries
	// saying so have not been cleaned up yet.
	data[SiteAddressesPrefix+"cp"] = []byte("10.10.0.10")
	data[SitePodCIDRsPrefix+"cp"] = []byte("10.244.242.64/26")

	peers, err := RemotePeers(data, "10.100.0.128", []string{"10.10.0.10"})
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}
	for _, prefix := range []string{"10.10.0.10/32", "10.244.242.64/26"} {
		if got := owners(peers, prefix); got != 1 {
			t.Errorf("%s is permitted on %d peers, want exactly 1", prefix, got)
		}
	}
}

// The relay carries the site, so it has to be a peer that is actually
// rendered. Handing the site to whichever node sorts lowest, before
// checking that node has a key, gives every one of those prefixes to
// nobody: the remote prunes its routes to the site and then cannot read
// the list that would put them back.
func TestRemotePeers_TheSiteGoesToARenderedPeer(t *testing.T) {
	data := peerSecret(
		map[string][2]string{
			"w1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.22/24"},
		},
		map[string]string{"w1": "10.244.190.64/26"},
	)
	// Selected, allocated an address, and its dialer has not published
	// a key yet: half a peer, and the lowest address of the two.
	data[NodePublicKeyPrefix+"w2"] = []byte("")
	data[NodeTunnelAddressPrefix+"w2"] = []byte("10.100.0.17/24")
	data[SiteAddressesPrefix+"cp"] = []byte("10.10.0.10")
	data[SitePodCIDRsPrefix+"cp"] = []byte("10.244.242.64/26")

	peers, err := RemotePeers(data, "10.100.0.128", []string{"10.10.0.10"})
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}
	for _, prefix := range []string{"10.10.0.10/32", "10.244.242.64/26"} {
		if got := owners(peers, prefix); got != 1 {
			t.Errorf("%s is permitted on %d peers, want exactly 1", prefix, got)
		}
	}
}

// The control-plane row: the only endpoint is the control plane, one
// worker is inside its retention window, and the other holds no tunnel.
// The control plane owns the API server's address as an ordinary node
// address, so nothing may claim it again on the peer designated to
// relay. The selected control plane also takes over transit from the retained worker.
func TestRemotePeers_AControlPlaneEndpointOwnsTheAPIAddressOnce(t *testing.T) {
	data := peerSecret(
		map[string][2]string{
			"cp": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.24/24"},
			"w1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.17/24"},
		},
		map[string]string{"cp": "10.244.242.64/26"},
	)
	data[NodeAddressesPrefix+"cp"] = []byte("10.10.0.10")
	// w1 is retained: it keeps its key and tunnel address, and its
	// prefixes have moved to the site entries.
	data[NodeDepartedAtPrefix+"w1"] = []byte("2026-09-05T19:33:42Z")
	data[SiteAddressesPrefix+"w1"] = []byte("10.10.0.11")
	data[SitePodCIDRsPrefix+"w1"] = []byte("10.244.190.64/26")
	// w2 never held a tunnel.
	data[SiteAddressesPrefix+"w2"] = []byte("10.10.0.12")
	data[SitePodCIDRsPrefix+"w2"] = []byte("10.244.80.192/26")

	peers, err := RemotePeers(data, "10.100.0.129", []string{"10.10.0.10"})
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}
	for _, prefix := range []string{
		"10.10.0.10/32", "10.244.242.64/26",
		"10.10.0.11/32", "10.244.190.64/26",
		"10.10.0.12/32", "10.244.80.192/26",
	} {
		if got := owners(peers, prefix); got != 1 {
			t.Errorf("%s is permitted on %d peers, want exactly 1", prefix, got)
		}
	}
}

// A site node with no tunnel reaches the remotes through the node that
// relays for it, and it must pick that node the way the render picks
// it, from the same data, or the two disagree and traffic dies at an
// accept list. This is derivation instead of convergence: no protocol
// carries the choice, so there is no window in which the choice is in
// flight.
func TestSiteTransit_FollowsTheRenderElection(t *testing.T) {
	data := map[string][]byte{
		// w1 is the relay: lowest tunnel address with a published key.
		"node-public-key-w1":     []byte("W1KEY"),
		"node-tunnel-address-w1": []byte("10.100.0.17/24"),
		"node-addresses-w1":      []byte("10.10.0.11"),
		"node-public-key-w2":     []byte("W2KEY"),
		"node-tunnel-address-w2": []byte("10.100.0.22/24"),
		"node-addresses-w2":      []byte("10.10.0.12"),
		// An allocation whose key has not landed is not a relay.
		"node-tunnel-address-early": []byte("10.100.0.5/24"),

		"peer-public-key-remote1":  []byte("R1KEY"),
		"peer-route-hosts-remote1": []byte("10.100.0.128,203.0.113.10"),
		"peer-allowed-ips-remote1": []byte("10.100.0.128/32,203.0.113.10/32,10.244.159.0/26"),
	}
	transit, err := SiteTransit(data, nil)
	if err != nil {
		t.Fatalf("SiteTransit: %v", err)
	}
	if transit == nil {
		t.Fatal("no transit derived, so a node with no tunnel has no path to any remote")
	}
	if transit.Via != "10.10.0.11" {
		t.Errorf("transit via %q, want the relay w1's node address 10.10.0.11: any other choice is a peer the remotes will not accept relayed sources from", transit.Via)
	}
	wantHosts := map[string]bool{"10.100.0.128": true, "203.0.113.10": true}
	for _, h := range transit.Hosts {
		delete(wantHosts, h)
	}
	if len(wantHosts) != 0 {
		t.Errorf("transit misses remote hosts %v", wantHosts)
	}
	if len(transit.Blocks) != 1 || transit.Blocks[0] != "10.244.159.0/26" {
		t.Errorf("transit blocks = %v, want the remote pod block alone", transit.Blocks)
	}
}

// A retained relay's addresses have already moved to the site entries.
// It is still the relay, and it is still reachable; the address is
// simply published under the other family.
func TestSiteTransit_ARetainedRelayIsStillReachable(t *testing.T) {
	data := map[string][]byte{
		"node-public-key-w1":     []byte("W1KEY"),
		"node-tunnel-address-w1": []byte("10.100.0.17/24"),
		"site-addresses-w1":      []byte("10.10.0.11"),

		"peer-public-key-remote1":  []byte("R1KEY"),
		"peer-route-hosts-remote1": []byte("10.100.0.128"),
		"peer-allowed-ips-remote1": []byte("10.100.0.128/32,10.244.159.0/26"),
	}
	transit, err := SiteTransit(data, nil)
	if err != nil {
		t.Fatalf("SiteTransit: %v", err)
	}
	if transit == nil || transit.Via != "10.10.0.11" {
		t.Fatalf("transit = %+v, want via 10.10.0.11 from the site entry", transit)
	}
}

// No relay, no transit: the caller reports nothing rather than
// guessing a next hop.
func TestSiteTransit_NoRelayMeansNoTransit(t *testing.T) {
	transit, err := SiteTransit(map[string][]byte{
		"node-tunnel-address-early": []byte("10.100.0.5/24"),
		"peer-public-key-remote1":   []byte("R1KEY"),
		"peer-route-hosts-remote1":  []byte("10.100.0.128"),
	}, nil)
	if err != nil {
		t.Fatalf("SiteTransit: %v", err)
	}
	if transit != nil {
		t.Fatalf("transit = %+v, want none: there is no endpoint to carry it", transit)
	}
}

// The render elects one local to carry what nobody owns: the API
// servers and the site nodes with no tunnel. That set rides the relay
// by election, not by ownership, and the applier on the far side is
// allowed to move exactly that set when its own kernel says the relay
// is dead. So the render has to say which prefixes those are; an
// applier guessing would be a second authority on ownership, and every
// defect this tree has fixed was two authorities disagreeing.
func TestRemotePeers_TheElectedRelayDeclaresItsTransit(t *testing.T) {
	data := peerSecret(
		map[string][2]string{
			"w1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"},
			"w2": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.2/24"},
		},
		map[string]string{"w1": "10.244.1.0/26", "w2": "10.244.2.0/26"},
	)
	data[NodeAddressesPrefix+"w1"] = []byte("10.10.0.11")
	data[NodeAddressesPrefix+"w2"] = []byte("10.10.0.12")
	data[SiteAddressesPrefix+"cp"] = []byte("10.10.0.10")
	data[SitePodCIDRsPrefix+"cp"] = []byte("10.244.0.0/26")

	peers, err := RemotePeers(data, "10.100.0.128", []string{"10.10.0.10"})
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(peers))
	}
	relay, other := peers[0], peers[1]
	if len(other.Transit) != 0 || len(other.TransitHosts) != 0 {
		t.Errorf("the unelected local declares transit %v %v; only the relay carries any", other.Transit, other.TransitHosts)
	}
	wantTransit := map[string]bool{"10.10.0.10/32": true, "10.244.0.0/26": true}
	for _, cidr := range relay.Transit {
		if !wantTransit[cidr] {
			t.Errorf("transit declares %s, which the relay owns or nobody claimed", cidr)
		}
		delete(wantTransit, cidr)
	}
	if len(wantTransit) != 0 {
		t.Errorf("transit misses %v", wantTransit)
	}
	wantHosts := map[string]bool{"10.10.0.10": true}
	for _, h := range relay.TransitHosts {
		if !wantHosts[h] {
			t.Errorf("transit hosts declare %s, which is not transit", h)
		}
		delete(wantHosts, h)
	}
	if len(wantHosts) != 0 {
		t.Errorf("transit hosts miss %v", wantHosts)
	}
	// The declaration is a labeling of the entry, not a second grant.
	allowed := map[string]bool{}
	for _, cidr := range relay.WGAllowedIPs {
		allowed[cidr] = true
	}
	for _, cidr := range relay.Transit {
		if !allowed[cidr] {
			t.Errorf("transit %s is not in the relay's accept list; the label and the grant disagree", cidr)
		}
	}
	hosts := map[string]bool{}
	for _, h := range relay.RouteHosts {
		hosts[h] = true
	}
	for _, h := range relay.TransitHosts {
		if !hosts[h] {
			t.Errorf("transit host %s is not in the relay's route hosts", h)
		}
	}
}

// A relay the API server calls NotReady is passed over: a node with no
// tunnel chooses its way out from the same record as everyone else,
// plus the one fact it legitimately owns, what the cluster says about
// its neighbours' health.
func TestSiteTransit_ANotReadyRelayIsPassedOver(t *testing.T) {
	data := map[string][]byte{
		"node-public-key-w1":       []byte("W1KEY"),
		"node-tunnel-address-w1":   []byte("10.100.0.17/24"),
		"node-addresses-w1":        []byte("10.10.0.11"),
		"node-public-key-w2":       []byte("W2KEY"),
		"node-tunnel-address-w2":   []byte("10.100.0.22/24"),
		"node-addresses-w2":        []byte("10.10.0.12"),
		"peer-public-key-remote1":  []byte("R1KEY"),
		"peer-route-hosts-remote1": []byte("10.100.0.128,203.0.113.10"),
		"peer-allowed-ips-remote1": []byte("10.100.0.128/32,203.0.113.10/32,10.244.159.0/26"),
	}
	transit, err := SiteTransit(data, map[string]bool{"w1": true})
	if err != nil {
		t.Fatalf("SiteTransit: %v", err)
	}
	if transit == nil {
		t.Fatal("no transit derived")
	}
	if transit.Via != "10.10.0.12" {
		t.Errorf("transit via %q, want the live w2's 10.10.0.12: routing through a dead relay is a black hole with a quorum of one", transit.Via)
	}
}

// Every candidate NotReady falls back to the render's election. A
// maybe-dead relay is a path that may come back; no relay is no path
// at all, and the report that produced the NotReady may itself be the
// thing that is stale.
func TestSiteTransit_AllNotReadyFallsBackToTheElection(t *testing.T) {
	data := map[string][]byte{
		"node-public-key-w1":       []byte("W1KEY"),
		"node-tunnel-address-w1":   []byte("10.100.0.17/24"),
		"node-addresses-w1":        []byte("10.10.0.11"),
		"node-public-key-w2":       []byte("W2KEY"),
		"node-tunnel-address-w2":   []byte("10.100.0.22/24"),
		"node-addresses-w2":        []byte("10.10.0.12"),
		"peer-public-key-remote1":  []byte("R1KEY"),
		"peer-route-hosts-remote1": []byte("10.100.0.128"),
		"peer-allowed-ips-remote1": []byte("10.100.0.128/32"),
	}
	transit, err := SiteTransit(data, map[string]bool{"w1": true, "w2": true})
	if err != nil {
		t.Fatalf("SiteTransit: %v", err)
	}
	if transit == nil || transit.Via != "10.10.0.11" {
		t.Fatalf("transit = %+v, want the rendered election's w1", transit)
	}
}
