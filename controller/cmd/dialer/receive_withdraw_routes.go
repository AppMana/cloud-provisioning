package main

import (
	"context"
	"fmt"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// withdrawReceiveDeviceRoutes removes only the explicitly admitted direct
// receiver routes. The caller must hold the host lock and have checkpointed a
// live withdrawal covering these generations. No handshake from the deleted
// worker is required. Device retirement remains a separate verified operation.
// Foreign tables, source exceptions and relay routes must be retired by their
// own owners first; this function never repairs them to make cleanup pass.
func withdrawReceiveDeviceRoutes(ctx context.Context, cfg config, generations []map[string]receiveRouteSelection, own []string) error {
	if cfg.routeTable <= unix.RT_TABLE_LOCAL || cfg.fwmark <= 0 || len(generations) == 0 {
		return fmt.Errorf("receiver withdrawal requires owned policy and admitted generations")
	}
	cfg.receiveRouteScope = map[string]bool{}
	cfg.receiveRoutes, cfg.receiveRelayRoutes = nil, nil
	indices := map[int]bool{}
	for _, generation := range generations {
		for raw, selection := range generation {
			cfg.receiveRouteScope[raw] = true
			link, err := netlink.LinkByName(selection.Interface)
			if isLinkNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			if selection.Interface == cfg.iface || link.Type() != "wireguard" || link.Attrs().Alias != selection.Alias {
				return fmt.Errorf("withdrawn receiver ownership changed")
			}
			addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
			if err != nil {
				return err
			}
			if len(addresses) != 0 {
				return fmt.Errorf("withdrawn receiver has address dependencies")
			}
			indices[link.Attrs().Index] = true
		}
	}
	if len(cfg.receiveRouteScope) == 0 {
		return nil
	}
	if err := validateReceiveRouteScope(cfg, nil, nil, nil, nil, nil); err != nil {
		return err
	}
	if err := verifyReceivePolicyRules(cfg); err != nil {
		return err
	}
	if err := preflightReceiveRouteGenerationsWithHandshake(cfg, generations, nil, own, false); err != nil {
		return err
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	var removing []netlink.Route
	for _, route := range routes {
		uses := false
		for index := range indices {
			if receiveRouteUsesLink(route, index) && !receiveDeviceKernelMulticast(route, index) {
				uses = true
			}
		}
		if !uses {
			continue
		}
		scope := netlink.SCOPE_LINK
		if route.Family == netlink.FAMILY_V6 {
			scope = netlink.SCOPE_UNIVERSE
		}
		if route.Table != cfg.routeTable || route.Dst == nil || !cfg.receiveRouteScope[route.Dst.String()] || route.Scope != scope || route.Flags != 0 {
			return fmt.Errorf("receiver withdrawal depends on an unowned route in table %d", route.Table)
		}
		// The shared preflight checked protocol, metric, source, next hops and
		// exact peer permissions before any route is removed.
		removing = append(removing, route)
	}
	for i := range removing {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := netlink.RouteDel(&removing[i]); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return verifyReceiveRouteDestinations(cfg, nil, nil, own)
}
