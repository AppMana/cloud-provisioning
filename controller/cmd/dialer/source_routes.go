package main

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
)

// The companion table contains only direct host routes for packets whose
// source is this node's tunnel address. Calico can address a VXLAN packet
// to a remote tunnel IP while binding its source to the physical node IP;
// a destination-only exception would send that packet to a bare peer that
// no longer accepts the physical source.
const tunnelSourceTableOffset = 65536

func tunnelSourceTable(table int) (int, error) {
	if table <= exemptRulePriority+1 || table > 2147483647-tunnelSourceTableOffset {
		return 0, fmt.Errorf("route table %d cannot reserve a source table and earlier rule", table)
	}
	return table + tunnelSourceTableOffset, nil
}

func clearTunnelSourceRoutes(table int, iface ...string) error {
	companion, err := tunnelSourceTable(table)
	if err != nil {
		return err
	}
	index := 0
	if len(iface) > 1 {
		return fmt.Errorf("ambiguous source route device")
	}
	if len(iface) == 1 {
		link, err := netlink.LinkByName(iface[0])
		if err != nil && !isLinkNotFound(err) {
			return err
		}
		if err == nil {
			index = link.Attrs().Index
		}
	}
	rules, routes, err := inspectTunnelSourceState(table, companion, index)
	if err != nil {
		return err
	}
	for i := range rules {
		if err := netlink.RuleDel(&rules[i]); err != nil {
			return err
		}
	}
	for i := range routes {
		if err := netlink.RouteDel(&routes[i]); err != nil {
			return err
		}
	}
	return nil
}

// Reconcile the complete source-exception set in one pass. Callers must supply
// every active family and destination; omission withdraws its prior exception.
func reconcileTunnelSourceRoutes(cfg config, localAddresses []string, hosts []net.IPNet) error {
	if len(hosts) == 0 {
		return clearTunnelSourceRoutes(cfg.routeTable, cfg.iface)
	}
	table, err := tunnelSourceTable(cfg.routeTable)
	if err != nil {
		return err
	}
	sources := map[int]*net.IPNet{}
	for _, raw := range localAddresses {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 || !prefix.Addr().IsGlobalUnicast() || prefix.Addr().Is4In6() {
			return fmt.Errorf("parsing tunnel source %q: explicit unicast allocation required", raw)
		}
		family, bits := netlink.FAMILY_V6, 128
		if prefix.Addr().Is4() {
			family, bits = netlink.FAMILY_V4, 32
		}
		if sources[family] != nil {
			return fmt.Errorf("multiple tunnel sources for one address family")
		}
		sources[family] = &net.IPNet{IP: prefix.Addr().AsSlice(), Mask: net.CIDRMask(bits, bits)}
	}
	desired := map[string]bool{}
	desiredFamilies := map[int]bool{}
	// Validate both families before touching the kernel. A complete projection
	// prevents one family's reconcile from withdrawing the other family's path.
	for _, host := range hosts {
		ones, size := host.Mask.Size()
		family := netlink.FAMILY_V6
		if host.IP.To4() != nil {
			family = netlink.FAMILY_V4
		}
		if ones != size || (size != 32 && size != 128) || (family == netlink.FAMILY_V4) != (size == 32) || !host.IP.IsGlobalUnicast() || sources[family] == nil {
			return fmt.Errorf("tunnel source route %s must be a same-family single host with an allocated source", host.String())
		}
		desired[host.String()] = true
		desiredFamilies[family] = true
	}
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return err
	}
	existingRules, existingRoutes, err := inspectTunnelSourceState(cfg.routeTable, table, link.Attrs().Index)
	if err != nil {
		return err
	}
	for i := range hosts {
		route := &netlink.Route{Dst: &hosts[i], LinkIndex: link.Attrs().Index, Scope: netlink.SCOPE_LINK, Table: table}
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("installing tunnel source route: %w", err)
		}
	}
	found := map[int]bool{}
	var stale []netlink.Rule
	for _, existing := range existingRules {
		family := existing.Family
		if desiredFamilies[family] && !found[family] && existing.Src.String() == sources[family].String() {
			found[family] = true
		} else {
			stale = append(stale, existing)
		}
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		if !desiredFamilies[family] || found[family] {
			continue
		}
		rule := netlink.NewRule()
		rule.Table, rule.Priority, rule.Family, rule.Src = table, cfg.routeTable-1, family, sources[family]
		if err := netlink.RuleAdd(rule); err != nil {
			return fmt.Errorf("installing tunnel source rule: %w", err)
		}
	}
	for i := range stale {
		if err := netlink.RuleDel(&stale[i]); err != nil {
			return err
		}
	}
	for i := range existingRoutes {
		if desired[existingRoutes[i].Dst.String()] {
			continue
		}
		if err := netlink.RouteDel(&existingRoutes[i]); err != nil {
			return err
		}
	}
	return nil
}
