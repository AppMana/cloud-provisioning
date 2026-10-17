package main

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Verify the installed receiver routes without repairing anything. This proves
// the selected table and device state at observation time, not arbitrary source,
// input-interface or external policy routing. Packet qualification remains
// necessary before a platform phase verifier can claim end-to-end forwarding.
func verifyReceiveRouteInstallation(cfg config, hosts, blocks []net.IPNet, own []string) error {
	if len(cfg.receiveRoutes) == 0 && len(cfg.receiveRelayRoutes) == 0 && cfg.receiveRouteScope == nil {
		return nil
	}
	selected, err := validateReceiveRouteSelections(cfg, hosts, blocks)
	if err != nil {
		return err
	}
	relays, err := validateReceiveRelayRoutes(cfg, hosts, blocks)
	if err != nil {
		return err
	}
	for raw, index := range relays {
		selected[raw] = index
	}
	if cfg.receiveRouteScope != nil {
		for _, list := range [][]net.IPNet{hosts, blocks} {
			for _, dst := range list {
				if _, receiver := selected[dst.String()]; receiver {
					continue
				}
				legacy, err := netlink.LinkByName(cfg.iface)
				if err != nil {
					return err
				}
				if legacy.Type() != "wireguard" {
					return fmt.Errorf("selected legacy route device is not WireGuard")
				}
				selected[dst.String()] = legacy.Attrs().Index
			}
		}
	}
	local := localIPs()
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		if err := validateTunnelRuleActions(family, cfg.routeTable, cfg.fwmark); err != nil {
			return err
		}
		rules, err := netlink.RuleList(family)
		if err != nil {
			return err
		}
		missing, err := planTunnelRules(rules, family, cfg.routeTable, cfg.fwmark)
		if err != nil {
			return err
		}
		if len(missing) != 0 {
			return fmt.Errorf("receiver policy rules are missing")
		}
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		for _, route := range routes {
			if route.Dst != nil && cfg.receiveRouteScope[route.Dst.String()] {
				if _, desired := selected[route.Dst.String()]; !desired {
					return fmt.Errorf("withdrawn receiver destination still has a route")
				}
			}
		}
		for raw, index := range selected {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				return err
			}
			if prefix.Addr().Is4() != (family == netlink.FAMILY_V4) {
				continue
			}
			count := 0
			for _, route := range routes {
				if route.Dst == nil {
					continue
				}
				actual, err := netip.ParsePrefix(route.Dst.String())
				if err != nil {
					return err
				}
				if actual != prefix {
					if actual.Addr().BitLen() == prefix.Addr().BitLen() && actual.Bits() > prefix.Bits() && prefix.Contains(actual.Addr()) {
						if _, explicit := selected[actual.String()]; !explicit {
							return fmt.Errorf("unselected more-specific route shadows receiver destination %s", raw)
						}
					}
					continue
				}
				count++
				source := transitSource(route.Dst.IP, own, local)
				if route.LinkIndex != index || route.Type != unix.RTN_UNICAST || route.Priority != receiveRoutePriority(route.Dst) || !route.Gw.Equal(net.ParseIP(cfg.receiveRelayRoutes[raw])) || len(route.MultiPath) != 0 || route.Via != nil || route.Encap != nil || route.NewDst != nil || route.Tos != 0 || !route.Src.Equal(source) {
					return fmt.Errorf("installed receiver route differs from selected destination %s", raw)
				}
			}
			if count != 1 {
				return fmt.Errorf("receiver destination %s has %d exact routes", raw, count)
			}
		}
	}
	return nil
}

// Linux normalizes a zero IPv6 unicast metric to IP6_RT_PRIO_USER. Use the
// resulting kernel value for both writes and ownership checks.
func receiveRoutePriority(dst *net.IPNet) int {
	if dst != nil && dst.IP.To4() == nil {
		return 1024
	}
	return 0
}
