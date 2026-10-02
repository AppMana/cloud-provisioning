package lab

import (
	"fmt"
	"strings"
)

// Identity sites give each site node a cluster address of its own, held on a
// dummy interface outside the LAN's IPv4 prefix, the way a site whose LAN
// addresses come from DHCP keeps a stable kubelet, API and Calico address.
//
// The IPv4 identities are not on-link anywhere, so every site node and the
// bastion route the identity prefix on-link over the LAN and answer for it
// by weak-host ARP. The IPv6 identities sit inside the LAN's own /64, so they
// are on-link already, and the node's LAN IPv6 address is a second address in
// that same /64: an autodetection method that selects by prefix can pick
// either, as it can on such a site.
//
// The site has no IPv6 WAN. The router forwards and masquerades IPv4 only,
// and only from the LAN prefix: an identity address leaving the site is
// dropped, as it would be by a home router that never heard of it.
const (
	IdentityPrefix4 = "10.101.0"
	IdentityCIDR4   = IdentityPrefix4 + ".0/24"
	SitePrefix6     = "fd8f:cf26:522a"
	SiteCIDR6       = SitePrefix6 + "::/64"
	// IdentityDevice is the dummy interface identities live on.
	IdentityDevice = "vip0"
)

// WithIdentities is one control plane that is also a worker, two workers and
// the given remote capacity, with identity addressing as described above.
func WithIdentities(remoteSlots int) (Topology, error) {
	topo, err := WithRemoteSlots(remoteSlots)
	if err != nil {
		return Topology{}, err
	}
	identities := map[string]int{"cp": 1, "w1": 2, "w2": 3}
	nodes := topo.Nodes[:0]
	for _, n := range topo.Nodes {
		switch {
		case n.Name == "cp2" || n.Name == "cp3":
			continue
		case n.Role == Bastion:
			n.Interfaces[0].Address6 = siteAddress6(n)
		case identities[n.Name] != 0:
			host := identities[n.Name]
			n.Interfaces[0].Address6 = siteAddress6(n)
			n.Identity = []string{
				fmt.Sprintf("%s.%d/32", IdentityPrefix4, host),
				fmt.Sprintf("%s::%d/128", SitePrefix6, host),
			}
		}
		nodes = append(nodes, n)
	}
	topo.Nodes = nodes
	topo.IdentityRoutes = []string{IdentityCIDR4}
	return topo, nil
}

// siteAddress6 numbers a LAN IPv6 address after the node's LAN IPv4 host
// number, away from the identities at the bottom of the /64.
func siteAddress6(n Node) string {
	v4 := n.Address(LANSegment)
	host := v4[strings.LastIndex(v4, ".")+1:]
	return fmt.Sprintf("%s::a:%s/64", SitePrefix6, host)
}

// ClusterAddresses are the addresses the cluster knows a node by: its
// identities where it has them, otherwise its LAN address.
func (n Node) ClusterAddresses() []string {
	if len(n.Identity) == 0 {
		if a := n.Address(LANSegment); a != "" {
			return []string{a}
		}
		return nil
	}
	out := make([]string, 0, len(n.Identity))
	for _, cidr := range n.Identity {
		addr, _, _ := strings.Cut(cidr, "/")
		out = append(out, addr)
	}
	return out
}

// ClusterAddress is the node's primary (IPv4) cluster address.
func (n Node) ClusterAddress() string {
	if addrs := n.ClusterAddresses(); len(addrs) > 0 {
		return addrs[0]
	}
	return ""
}

// DualStack reports whether the site addresses its nodes in both families.
func (t Topology) DualStack() bool {
	for _, n := range t.NodesInRole(ControlPlane, Worker) {
		if len(n.ClusterAddresses()) > 1 {
			return true
		}
	}
	return false
}
