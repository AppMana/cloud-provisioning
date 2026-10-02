package lab

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestIdentitySiteIsOneControllerAndTwoWorkers(t *testing.T) {
	topo, err := WithIdentities(2)
	if err != nil {
		t.Fatal(err)
	}
	var cps, workers, remotes []string
	for _, n := range topo.NodesInRole(ControlPlane) {
		cps = append(cps, n.Name)
	}
	for _, n := range topo.NodesInRole(Worker) {
		workers = append(workers, n.Name)
	}
	for _, n := range topo.NodesInRole(Remote) {
		remotes = append(remotes, n.Name)
	}
	if !reflect.DeepEqual(cps, []string{"cp"}) || !reflect.DeepEqual(workers, []string{"w1", "w2"}) || !reflect.DeepEqual(remotes, []string{"remote1", "remote2"}) {
		t.Fatalf("control planes %v, workers %v, remotes %v", cps, workers, remotes)
	}
	if !topo.DualStack() {
		t.Fatal("identity site is not dual-stack")
	}
	if Default().DualStack() {
		t.Fatal("the default site became dual-stack")
	}
}

// The identities must not be the LAN address, must not be on-link by prefix
// for IPv4 (that is what the on-link route is for), and must be on-link for
// IPv6, where the LAN /64 contains them.
func TestIdentitiesSitOutsideTheLANForIPv4AndInsideItForIPv6(t *testing.T) {
	topo, err := WithIdentities(2)
	if err != nil {
		t.Fatal(err)
	}
	lan4 := netip.MustParsePrefix(SitePrefix + ".0/24")
	identity4 := netip.MustParsePrefix(IdentityCIDR4)
	site6 := netip.MustParsePrefix(SiteCIDR6)
	seen := map[netip.Addr]string{}
	for _, n := range topo.NodesInRole(ControlPlane, Worker) {
		addrs := n.ClusterAddresses()
		if len(addrs) != 2 {
			t.Fatalf("%s has cluster addresses %v", n.Name, addrs)
		}
		v4, v6 := netip.MustParseAddr(addrs[0]), netip.MustParseAddr(addrs[1])
		if !v4.Is4() || !v6.Is6() || lan4.Contains(v4) || !identity4.Contains(v4) || !site6.Contains(v6) {
			t.Fatalf("%s identities %v", n.Name, addrs)
		}
		if n.ClusterAddress() != addrs[0] || n.ClusterAddress() == n.Address(LANSegment) {
			t.Fatalf("%s cluster address %s", n.Name, n.ClusterAddress())
		}
		lan6 := netip.MustParsePrefix(n.Interfaces[0].Address6)
		if lan6.Bits() != 64 || lan6.Masked() != site6 || lan6.Addr() == v6 {
			t.Fatalf("%s LAN IPv6 %s", n.Name, lan6)
		}
		for _, a := range []netip.Addr{v4, v6, lan6.Addr()} {
			if prior, ok := seen[a]; ok {
				t.Fatalf("%s and %s both hold %s", prior, n.Name, a)
			}
			seen[a] = n.Name
		}
	}
	if !reflect.DeepEqual(topo.IdentityRoutes, []string{IdentityCIDR4}) {
		t.Fatalf("identity routes %v", topo.IdentityRoutes)
	}
	// The bastion is an operator host on the LAN: no identity, but an
	// IPv6 address and the same on-link route as the nodes.
	bastion := topo.MustNode("bastion")
	if len(bastion.Identity) != 0 || bastion.Interfaces[0].Address6 == "" {
		t.Fatalf("bastion %+v", bastion)
	}
	// Remotes are in clouds with IPv4 only and have no identity.
	for _, n := range topo.NodesInRole(Remote) {
		if len(n.Identity) != 0 || n.Interfaces[0].Address6 != "" {
			t.Fatalf("remote %s %+v", n.Name, n)
		}
	}
}

func TestDefaultSiteNodesAreKnownByTheirLANAddress(t *testing.T) {
	for _, n := range Default().NodesInRole(ControlPlane, Worker) {
		if !reflect.DeepEqual(n.ClusterAddresses(), []string{n.Address(LANSegment)}) {
			t.Fatalf("%s cluster addresses %v", n.Name, n.ClusterAddresses())
		}
	}
}
