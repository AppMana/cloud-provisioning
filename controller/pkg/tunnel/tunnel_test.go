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
