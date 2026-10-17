package main

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
)

func validateTunnelSourceScope(cfg config, hosts []net.IPNet) error {
	if cfg.receiveRouteScope == nil {
		return nil
	}
	if err := validateReceiveRouteScope(cfg, nil, nil, nil, nil, nil); err != nil {
		return err
	}
	for _, host := range hosts {
		if !tunnelSourceInScope(cfg.receiveRouteScope, host) {
			return fmt.Errorf("source exception exceeds admitted journal scope")
		}
	}
	return nil
}

// Source exceptions are host routes. A journal owning a destination block also
// owns any source exception within that block; unexpected hosts cannot hide
// outside scope merely because their prefix is more specific.
func tunnelSourceInScope(scope map[string]bool, host net.IPNet) bool {
	dst, err := netip.ParsePrefix(host.String())
	if err != nil {
		return false
	}
	for raw, owned := range scope {
		prefix, err := netip.ParsePrefix(raw)
		if err == nil && owned && prefix.Bits() <= dst.Bits() && prefix.Contains(dst.Addr()) {
			return true
		}
	}
	return false
}

// Carry outside-scope routes forward unchanged; never manufacture a selector
// for them. Their existing shared selector must already match the admitted
// allocation. Otherwise a scoped update could silently change another worker's
// accepted source, or repair it without authority. The returned desired union
// is used for pruning and final preservation checks, not route installation.
func preserveUnscopedTunnelSources(cfg config, addresses []string, sources map[int]*net.IPNet, desired map[string]bool, rules []netlink.Rule, routes []netlink.Route) error {
	if cfg.receiveRouteScope == nil {
		return nil
	}
	allocated, err := tunnelSourceAllocations(addresses)
	if err != nil {
		return err
	}
	preservedFamilies := map[int]bool{}
	for _, route := range routes {
		if tunnelSourceInScope(cfg.receiveRouteScope, *route.Dst) {
			continue
		}
		family := netlink.FAMILY_V6
		if route.Dst.IP.To4() != nil {
			family = netlink.FAMILY_V4
		}
		preservedFamilies[family] = true
		desired[route.Dst.String()] = true
	}
	for family := range preservedFamilies {
		expected := allocated[family]
		if expected == nil {
			return fmt.Errorf("unscoped source exception has no admitted allocation")
		}
		count := 0
		for _, rule := range rules {
			if rule.Family != family {
				continue
			}
			if rule.Src == nil || rule.Src.String() != expected.String() {
				return fmt.Errorf("unscoped source exception uses another allocation")
			}
			count++
		}
		if count != 1 {
			return fmt.Errorf("unscoped source exception lacks one existing selector")
		}
		sources[family] = expected
	}
	return nil
}
