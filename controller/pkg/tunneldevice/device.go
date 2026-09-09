// Package tunneldevice validates the shared peer contract before an OS backend
// touches interfaces or routes. AllowedIPs remain distinct from kernel routes.
package tunneldevice

import (
	"fmt"
	"net/netip"
	"sort"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Device owns one dedicated tunnel adapter, never the physical NIC or CNI.
type Device interface {
	Apply(tunnel.PeersFileDoc, uint16) error
	Close() error
}

type Peer struct {
	Key      wgtypes.Key
	Endpoint netip.AddrPort
	Allowed  []netip.Prefix
}
type Plan struct {
	Key     wgtypes.Key
	Address netip.Prefix
	Peers   []Peer
	Routes  []netip.Prefix
}

// Compile validates the entire document before the first platform mutation.
// Numeric endpoints avoid DNS being dependent on the tunnel being configured.
func Compile(doc tunnel.PeersFileDoc) (Plan, error) {
	var p Plan
	var err error
	p.Key, err = wgtypes.ParseKey(doc.PrivateKey)
	if err != nil || p.Key == (wgtypes.Key{}) {
		return p, fmt.Errorf("invalid private key")
	}
	p.Address, err = netip.ParsePrefix(doc.LocalAddress)
	if err != nil || p.Address.Addr().Is4In6() || p.Address.Bits() != p.Address.Addr().BitLen() {
		return p, fmt.Errorf("local address must be a host prefix")
	}
	keys := map[wgtypes.Key]bool{}
	prefixes := map[netip.Prefix]bool{}
	routes := map[netip.Prefix]bool{}
	for i, spec := range doc.Peers {
		peer := Peer{}
		peer.Key, err = wgtypes.ParseKey(spec.PublicKey)
		if err != nil || peer.Key == (wgtypes.Key{}) || peer.Key == p.Key.PublicKey() || keys[peer.Key] {
			return p, fmt.Errorf("peer %d has invalid or duplicate key", i)
		}
		keys[peer.Key] = true
		if spec.Endpoint != "" {
			peer.Endpoint, err = netip.ParseAddrPort(spec.Endpoint)
			if err != nil || peer.Endpoint.Port() == 0 || peer.Endpoint.Addr().Is4In6() {
				return p, fmt.Errorf("peer %d has invalid endpoint", i)
			}
		}
		for _, raw := range spec.WGAllowedIPs {
			prefix, e := netip.ParsePrefix(raw)
			if e != nil || prefix.Addr().Is4In6() || prefix != prefix.Masked() || prefixes[prefix] {
				return p, fmt.Errorf("peer %d has invalid or duplicate allowed prefix", i)
			}
			prefixes[prefix] = true
			peer.Allowed = append(peer.Allowed, prefix)
		}
		for _, raw := range spec.AllRouteHosts() {
			host, e := netip.ParsePrefix(raw)
			if e != nil {
				// RemotePeers publishes bare addresses in the shared contract;
				// canonicalize those to host prefixes before native routing.
				if address, parseErr := netip.ParseAddr(raw); parseErr == nil {
					host = netip.PrefixFrom(address, address.BitLen())
					e = nil
				}
			}
			if e != nil || host.Addr().Is4In6() || host.Bits() != host.Addr().BitLen() || host == p.Address {
				return p, fmt.Errorf("peer %d route must identify a remote host", i)
			}
			allowed := false
			for _, prefix := range peer.Allowed {
				if prefix.Contains(host.Addr()) {
					allowed = true
				}
			}
			if !allowed {
				return p, fmt.Errorf("peer %d route is not permitted by its allowed IPs", i)
			}
			routes[host] = true
		}
		p.Peers = append(p.Peers, peer)
	}
	for route := range routes {
		p.Routes = append(p.Routes, route)
	}
	sort.Slice(p.Routes, func(i, j int) bool { return p.Routes[i].String() < p.Routes[j].String() })
	return p, nil
}

// RouteObserver lets the host verify unchanged desired state without replacing
// WireGuard peers or resetting their live sessions.
type RouteObserver interface {
	Verify(tunnel.PeersFileDoc) error
}

func VerifyRoutes(desired []netip.Prefix, owned map[netip.Prefix]bool, observed []netip.Prefix) error {
	present := map[netip.Prefix]bool{}
	wanted := map[netip.Prefix]bool{}
	for _, p := range observed {
		present[p] = true
	}
	for _, p := range desired {
		wanted[p] = true
		if !present[p] {
			return fmt.Errorf("desired kernel route %s is missing", p)
		}
	}
	for p := range owned {
		if !wanted[p] && present[p] {
			return fmt.Errorf("retired kernel route %s is still present", p)
		}
	}
	return nil
}
