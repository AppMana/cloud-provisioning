package main

import (
	"fmt"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func matchesTunnelSourceRule(rule netlink.Rule, family, table, priority int) bool {
	if rule.Src == nil {
		return false
	}
	source, err := netip.ParsePrefix(rule.Src.String())
	if err != nil || source.Bits() != source.Addr().BitLen() || !source.Addr().IsGlobalUnicast() || source.Addr().Is4In6() || source.Addr().Is4() != (family == netlink.FAMILY_V4) {
		return false
	}
	rule.Src = nil
	return matchesTunnelRule(rule, family, table, priority, 0)
}

// Inspect both families before any mutation. A source lookup must have exactly
// the owned host-source shape. Table or priority collisions are never repaired
// by deleting the conflicting policy. A missing device authorizes no route
// deletion; its owned routes should already have disappeared with the device.
func inspectTunnelSourceState(base, table, index int, alternates ...map[string]int) ([]netlink.Rule, []netlink.Route, error) {
	var ownedRules []netlink.Rule
	var ownedRoutes []netlink.Route
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		if err := validatePolicyRuleActions(family, base-1, 0, true); err != nil {
			return nil, nil, err
		}
		rules, err := netlink.RuleList(family)
		if err != nil {
			return nil, nil, err
		}
		selected, err := selectTunnelSourceRules(rules, family, table, base-1)
		if err != nil {
			return nil, nil, err
		}
		ownedRules = append(ownedRules, selected...)
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return nil, nil, err
		}
		for _, route := range routes {
			ownedIndex := index > 0 && route.LinkIndex == index
			if route.Dst != nil {
				for _, byDestination := range alternates {
					ownedIndex = ownedIndex || (route.LinkIndex > 0 && route.LinkIndex == byDestination[route.Dst.String()])
				}
			}
			if route.Dst == nil || !ownedIndex || route.Type != unix.RTN_UNICAST || route.Protocol != unix.RTPROT_BOOT || route.Priority != receiveRoutePriority(route.Dst) || len(route.Src) != 0 || len(route.Gw) != 0 || len(route.MultiPath) != 0 || route.Via != nil || route.Encap != nil || route.NewDst != nil || route.Tos != 0 {
				return nil, nil, fmt.Errorf("foreign route collides with tunnel source exception")
			}
			dst, err := netip.ParsePrefix(route.Dst.String())
			if err != nil || dst.Bits() != dst.Addr().BitLen() || !dst.Addr().IsGlobalUnicast() || dst.Addr().Is4In6() {
				return nil, nil, fmt.Errorf("non-host route in tunnel source exception table")
			}
			ownedRoutes = append(ownedRoutes, route)
		}
	}
	return ownedRules, ownedRoutes, nil
}

func selectTunnelSourceRules(rules []netlink.Rule, family, table, priority int) ([]netlink.Rule, error) {
	var owned []netlink.Rule
	seen := map[string]bool{}
	for _, rule := range rules {
		if rule.Table != table && rule.Priority != priority {
			continue
		}
		if !matchesTunnelSourceRule(rule, family, table, priority) {
			return nil, fmt.Errorf("foreign policy collides with tunnel source exception")
		}
		identity := rule.Src.String()
		if seen[identity] {
			return nil, fmt.Errorf("duplicate tunnel source exception")
		}
		seen[identity] = true
		owned = append(owned, rule)
	}
	return owned, nil
}
