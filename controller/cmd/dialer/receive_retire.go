package main

import (
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// withdrawLegacyReceivePeer is the native, peer-scoped retirement operation.
// The caller must hold the reconciliation fence and authorize retirement from
// committed drain receipts, with replacement routes already verified. This
// operation supplies neither that authorization nor permission to delete the
// shared interface. Normal reconciliation must not republish a retired peer.
func withdrawLegacyReceivePeer(devices receiverDevices, iface, publicKey, peerKey string, sources []string, allocated ...string) error {
	local, err := wgtypes.ParseKey(publicKey)
	if err != nil || local == (wgtypes.Key{}) {
		return fmt.Errorf("legacy retirement requires a device identity")
	}
	peer, err := wgtypes.ParseKey(peerKey)
	if err != nil || peer == (wgtypes.Key{}) || peer == local || devices == nil {
		return fmt.Errorf("legacy retirement requires a distinct peer identity")
	}
	prefixes := make([]netip.Prefix, 0, len(sources))
	seen := map[netip.Prefix]bool{}
	for _, source := range sources {
		p, err := netip.ParsePrefix(source)
		if err != nil || p != p.Masked() || p.Bits() == 0 || p.Addr().Is4In6() || !p.Addr().IsGlobalUnicast() || seen[p] {
			return fmt.Errorf("legacy retirement requires explicit canonical source permissions")
		}
		seen[p] = true
		prefixes = append(prefixes, p)
	}
	if len(prefixes) == 0 {
		return fmt.Errorf("legacy retirement requires source permissions")
	}
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return err // Absence is not permission to adopt another device later.
	}
	if link.Type() != "wireguard" {
		return fmt.Errorf("legacy retirement target is not WireGuard")
	}
	live, err := devices.Device(iface)
	if err != nil {
		return err
	}
	if live.PublicKey != local {
		return fmt.Errorf("legacy retirement device identity changed")
	}
	// Only allocation prefixes admitted by the phase executor can explain a
	// shared connected route. Kernel discovery alone does not grant an exception.
	addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return err
	}
	admitted := make([]netip.Prefix, 0, len(allocated))
	for _, raw := range allocated {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 || prefix.Addr().Is4In6() || !prefix.Addr().IsGlobalUnicast() {
			return fmt.Errorf("invalid retirement allocation")
		}
		admitted = append(admitted, prefix)
	}
	found := false
	for _, current := range live.Peers {
		if current.PublicKey == peer {
			if !receiveSourcesMatch(sources, current.AllowedIPs) {
				return fmt.Errorf("legacy retirement source permissions changed")
			}
			found = true
			continue
		}
		for _, allowed := range current.AllowedIPs {
			p, err := netip.ParsePrefix(allowed.String())
			if err != nil {
				return err
			}
			for _, source := range prefixes {
				if p.Overlaps(source) {
					return fmt.Errorf("retired source permissions overlap another legacy peer")
				}
			}
		}
	}
	// Inspect all tables and multipath next hops. Even a more-specific or broader
	// legacy route can still depend on this peer; route-table ownership alone
	// cannot prove the old path has drained. Do not alter foreign routes here.
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if !receiveRouteUsesLink(route, link.Attrs().Index) {
			continue
		}
		if receiveSharedConnectedRoute(route, link.Attrs().Index, admitted, addresses) {
			continue
		}
		if route.Dst == nil {
			return fmt.Errorf("legacy retirement is blocked by a default route")
		}
		destination, err := netip.ParsePrefix(route.Dst.String())
		if err != nil {
			return err
		}
		for _, source := range prefixes {
			if destination.Overlaps(source) {
				return fmt.Errorf("legacy retirement is blocked by route %s in table %d", destination, route.Table)
			}
		}
	}
	if found {
		// A targeted removal preserves all other peers, local addresses, listener
		// state and device identity. Never use ReplacePeers for a per-worker round.
		if err := devices.ConfigureDevice(iface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: peer, Remove: true}}}); err != nil {
			return err
		}
	}
	current, err := netlink.LinkByName(iface)
	if err != nil {
		return err
	}
	if current.Type() != "wireguard" || current.Attrs().Index != link.Attrs().Index {
		return fmt.Errorf("legacy device changed during retirement")
	}
	after, err := devices.Device(iface)
	if err != nil {
		return err
	}
	if after.PublicKey != local || after.ListenPort != live.ListenPort || after.FirewallMark != live.FirewallMark {
		return fmt.Errorf("legacy device configuration changed during retirement")
	}
	remaining := make(map[wgtypes.Key]wgtypes.Peer, len(after.Peers))
	for _, current := range after.Peers {
		if current.PublicKey == peer {
			return fmt.Errorf("retired peer remains on legacy device")
		}
		remaining[current.PublicKey] = current
	}
	for _, before := range live.Peers {
		if before.PublicKey == peer {
			continue
		}
		current, ok := remaining[before.PublicKey]
		allowed := make([]string, len(before.AllowedIPs))
		for i, prefix := range before.AllowedIPs {
			allowed[i] = prefix.String()
		}
		actual := make([]string, len(current.AllowedIPs))
		for i, prefix := range current.AllowedIPs {
			actual[i] = prefix.String()
		}
		slices.Sort(allowed)
		slices.Sort(actual)
		if !ok || !slices.Equal(allowed, actual) || before.PresharedKey != current.PresharedKey || before.PersistentKeepaliveInterval != current.PersistentKeepaliveInterval {
			return fmt.Errorf("another legacy peer changed during retirement")
		}
		delete(remaining, before.PublicKey)
	}
	if len(remaining) != 0 {
		return fmt.Errorf("legacy membership changed during retirement")
	}
	return nil
}

// The address-created route belongs to the shared device, not to an individual
// peer. The caller has already verified the replacement policy routes. Keep
// this route and its address for other workers; all explicit routes still fence
// peer removal, including broader routes in foreign tables.
func receiveSharedConnectedRoute(route netlink.Route, index int, allocated []netip.Prefix, addresses []netlink.Addr) bool {
	if route.LinkIndex != index || route.Table != unix.RT_TABLE_MAIN || route.Protocol != unix.RTPROT_KERNEL || route.Type != unix.RTN_UNICAST || route.Dst == nil || len(route.Gw) != 0 || len(route.MultiPath) != 0 || route.Via != nil || route.Encap != nil || route.NewDst != nil || route.Tos != 0 || route.Flags != 0 {
		return false
	}
	for _, prefix := range allocated {
		if route.Dst.String() != prefix.Masked().String() {
			continue
		}
		if prefix.Addr().Is4() {
			if route.Family != netlink.FAMILY_V4 || route.Scope != netlink.SCOPE_LINK || route.Priority != 0 || !route.Src.Equal(net.ParseIP(prefix.Addr().String())) {
				continue
			}
		} else if route.Family != netlink.FAMILY_V6 || route.Scope != netlink.SCOPE_UNIVERSE || route.Priority != 256 || len(route.Src) != 0 {
			continue
		}
		for _, address := range addresses {
			if address.IPNet == nil {
				continue
			}
			bits, _ := address.Mask.Size()
			if bits == prefix.Bits() && address.IP.Equal(net.ParseIP(prefix.Addr().String())) && address.Flags&unix.IFA_F_NOPREFIXROUTE == 0 {
				return true
			}
		}
	}
	return false
}
