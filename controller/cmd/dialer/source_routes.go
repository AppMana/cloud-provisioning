package main

import (
	"fmt"
	"net"

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

func clearTunnelSourceRoutes(table int) error {
	companion, err := tunnelSourceTable(table)
	if err != nil {
		return err
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleListFiltered(family, &netlink.Rule{Table: companion}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		for i := range rules {
			if err := netlink.RuleDel(&rules[i]); err != nil {
				return err
			}
		}
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: companion}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		for i := range routes {
			if err := netlink.RouteDel(&routes[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func reconcileTunnelSourceRoutes(cfg config, localAddress string, hosts []net.IPNet) error {
	if len(hosts) == 0 {
		return clearTunnelSourceRoutes(cfg.routeTable)
	}
	table, err := tunnelSourceTable(cfg.routeTable)
	if err != nil {
		return err
	}
	ip, _, err := net.ParseCIDR(localAddress)
	if err != nil {
		return fmt.Errorf("parsing tunnel source: %w", err)
	}
	family, bits := netlink.FAMILY_V6, 128
	if ip.To4() != nil {
		ip, family, bits = ip.To4(), netlink.FAMILY_V4, 32
	}
	source := &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
	desired := map[string]bool{}
	// Validate the entire plan before touching the kernel.
	for _, host := range hosts {
		ones, size := host.Mask.Size()
		if ones != size || size != bits {
			return fmt.Errorf("tunnel source route %s must be a same-family single host", host.String())
		}
		desired[host.String()] = true
	}
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return err
	}
	for i := range hosts {
		route := &netlink.Route{Dst: &hosts[i], LinkIndex: link.Attrs().Index, Scope: netlink.SCOPE_LINK, Table: table}
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("installing tunnel source route: %w", err)
		}
	}
	rule := netlink.NewRule()
	rule.Table, rule.Priority, rule.Family, rule.Src = table, cfg.routeTable-1, family, source
	found := false
	var stale []netlink.Rule
	for _, f := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleListFiltered(f, &netlink.Rule{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		for _, existing := range rules {
			if !found && existing.Family == family && existing.Priority == rule.Priority && existing.Src != nil && existing.Src.String() == source.String() && existing.Dst == nil && existing.Mark == 0 && !existing.Invert && existing.IifName == "" && existing.OifName == "" {
				found = true
			} else {
				stale = append(stale, existing)
			}
		}
	}
	if !found {
		if err := netlink.RuleAdd(rule); err != nil {
			return fmt.Errorf("installing tunnel source rule: %w", err)
		}
	}
	for i := range stale {
		if err := netlink.RuleDel(&stale[i]); err != nil {
			return err
		}
	}
	for _, f := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := netlink.RouteListFiltered(f, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		for i := range routes {
			if routes[i].Dst != nil && desired[routes[i].Dst.String()] {
				continue
			}
			if err := netlink.RouteDel(&routes[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
