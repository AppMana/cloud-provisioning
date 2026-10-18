package main

import (
	"context"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// Withdraw only explicitly admitted source destinations. The caller holds the
// host lock and live withdrawal authority, and supplies exact recorded receiver
// candidates. This never installs routes, repairs policy or deletes devices.
func withdrawReceiveSourceRoutes(ctx context.Context, cfg config, addresses []string, admitted []net.IPNet) error {
	if cfg.receiveRouteScope == nil || len(cfg.receiveRouteScope) == 0 || len(cfg.receiveSourceRoutes) != 0 {
		return fmt.Errorf("source withdrawal requires explicit scope and no forwarding selection")
	}
	if err := validateTunnelSourceScope(cfg, admitted); err != nil {
		return err
	}
	table, err := tunnelSourceTable(cfg.routeTable)
	if err != nil {
		return err
	}
	allocation, err := tunnelSourceAllocations(addresses)
	if err != nil {
		return err
	}
	_, permitted, err := planTunnelSourceRoutes(addresses, admitted)
	if err != nil {
		return err
	}
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return err
	}
	if link.Type() != "wireguard" {
		return fmt.Errorf("source withdrawal requires the owned WireGuard device")
	}
	rules, routes, err := inspectConfiguredTunnelSourcesWithHandshake(cfg, table, link.Attrs().Index, false)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if allocation[rule.Family] == nil || rule.Src.String() != allocation[rule.Family].String() {
			return fmt.Errorf("source withdrawal selector differs from admitted allocation")
		}
	}
	wanted := map[string]bool{}
	sources := map[int]*net.IPNet{}
	if err := preserveUnscopedTunnelSources(cfg, addresses, sources, wanted, rules, routes); err != nil {
		return err
	}
	indices := map[string]int{}
	preserveUnscopedSourceIndices(cfg, indices, routes)
	var removing []netlink.Route
	for _, route := range routes {
		if !tunnelSourceInScope(cfg.receiveRouteScope, *route.Dst) {
			continue
		}
		if !permitted[route.Dst.String()] {
			return fmt.Errorf("source withdrawal found an uncommitted destination")
		}
		removing = append(removing, route)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Remove routes before selectors. A partial failure can leave an empty
	// selector, but never removes a selector still needed by a surviving route.
	for i := range removing {
		if err := netlink.RouteDel(&removing[i]); err != nil {
			return err
		}
	}
	for i := range rules {
		if sources[rules[i].Family] == nil {
			if err := netlink.RuleDel(&rules[i]); err != nil {
				return err
			}
		}
	}
	return verifyTunnelSourceSelection(cfg.routeTable, table, link.Attrs().Index, sources, wanted, indices)
}
