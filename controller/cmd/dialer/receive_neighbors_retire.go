package main

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Retire only the ordinary owners admitted from durable intent and a complete
// live mesh/journal read. The caller holds the reconciliation lock. A missing
// handshake is never deletion authority. Routes drain before source permissions;
// retries can finish that sequence without changing unrelated peers or policy.
func retireReceiveNeighbors(ctx context.Context, devices receiverDevices, cfg config, key wgtypes.Key, retiring []tunnel.PeerSpec, own, allocated []string) error {
	if len(retiring) == 0 {
		return nil
	}
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return err
	}
	live, err := devices.Device(cfg.iface)
	if err != nil {
		return err
	}
	if link.Type() != "wireguard" || key == (wgtypes.Key{}) || live.PublicKey != key.PublicKey() || cfg.routeTable <= unix.RT_TABLE_LOCAL || cfg.fwmark <= 0 || live.FirewallMark != cfg.fwmark {
		return fmt.Errorf("neighbor retirement device identity changed")
	}
	cfg.receiveRouteScope = map[string]bool{}
	cfg.receiveRoutes, cfg.receiveRelayRoutes = nil, nil
	owners := map[wgtypes.Key]tunnel.PeerSpec{}
	sourceOwners := map[netip.Prefix]wgtypes.Key{}
	var prefixes []netip.Prefix
	for _, peer := range retiring {
		public, err := wgtypes.ParseKey(peer.PublicKey)
		if err != nil || public == key.PublicKey() || public == (wgtypes.Key{}) || !peer.Remote || len(peer.Transit) != 0 || len(peer.TransitHosts) != 0 || len(peer.WGAllowedIPs) == 0 {
			return fmt.Errorf("invalid retiring neighbor identity")
		}
		if _, duplicate := owners[public]; duplicate {
			return fmt.Errorf("duplicate retiring neighbor")
		}
		owners[public] = peer
		for _, raw := range peer.WGAllowedIPs {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil || prefix != prefix.Masked() || !prefix.Addr().IsGlobalUnicast() || prefix.Addr().Is4In6() || prefix.Bits() == 0 {
				return fmt.Errorf("invalid retiring neighbor source")
			}
			for previous, owner := range sourceOwners {
				if prefix == previous || (prefix.Overlaps(previous) && owner != public) {
					return fmt.Errorf("retiring neighbor sources overlap")
				}
			}
			sourceOwners[prefix] = public
			prefixes = append(prefixes, prefix)
			cfg.receiveRouteScope[raw] = true
		}
	}
	for _, peer := range live.Peers {
		if owner, retiring := owners[peer.PublicKey]; retiring {
			if !receiveSourcesMatch(owner.WGAllowedIPs, peer.AllowedIPs) {
				return fmt.Errorf("retiring neighbor source permissions changed")
			}
			continue
		}
		for _, allowed := range peer.AllowedIPs {
			prefix, err := netip.ParsePrefix(allowed.String())
			if err != nil {
				return err
			}
			for _, retired := range prefixes {
				if prefix.Overlaps(retired) {
					return fmt.Errorf("neighbor retirement overlaps another live owner")
				}
			}
		}
	}
	if err := preflightReceiveRoutePhase(cfg, nil, own); err != nil {
		return err
	}
	if err := verifyReceivePolicyRules(cfg); err != nil {
		return err
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return err
	}
	var shared []netip.Prefix
	for _, raw := range allocated {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 || prefix.Addr().Is4In6() || !prefix.Addr().IsGlobalUnicast() {
			return fmt.Errorf("invalid neighbor retirement allocation")
		}
		shared = append(shared, prefix)
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if !receiveRouteUsesLink(route, link.Attrs().Index) || receiveSharedConnectedRoute(route, link.Attrs().Index, shared, addresses) {
			continue
		}
		if route.Dst == nil {
			return fmt.Errorf("neighbor retirement is blocked by a default route")
		}
		// Linux normalizes IPv6 unicast routes to universe scope even when
		// RouteReplace requested link scope. Require the actual family-specific
		// shape; the preceding preflight still checks protocol, metric, source,
		// interface and next-hop ownership.
		scope := netlink.SCOPE_LINK
		if route.Family == netlink.FAMILY_V6 {
			scope = netlink.SCOPE_UNIVERSE
		}
		if route.Table == cfg.routeTable && cfg.receiveRouteScope[route.Dst.String()] && route.Scope == scope && route.Flags == 0 {
			continue // The ownership preflight validated this exact route.
		}
		prefix, err := netip.ParsePrefix(route.Dst.String())
		if err != nil {
			return err
		}
		for _, retired := range prefixes {
			if prefix.Overlaps(retired) {
				return fmt.Errorf("neighbor retirement depends on unowned route %s in table %d (protocol=%d scope=%d flags=%d)", route.Dst, route.Table, route.Protocol, route.Scope, route.Flags)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := installRoutesWithPolicy(cfg, nil, nil, nil, nil, nil, own, false); err != nil {
		return err
	}
	for _, peer := range retiring {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := withdrawLegacyReceivePeer(devices, cfg.iface, key.PublicKey().String(), peer.PublicKey, peer.WGAllowedIPs, allocated...); err != nil {
			return err
		}
	}
	return ctx.Err()
}
