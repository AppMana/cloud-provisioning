package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// restoreReceivePolicyRules restores only shared lookup rules. The caller must
// hold the reconciliation lock and admit the complete live mesh and journal set.
// Every existing table route must match that set before the table is activated.
// Missing routes and peers are not recreated, and journal phases are not replayed.
func restoreReceivePolicyRules(ctx context.Context, devices receiverDevices, cfg config, key wgtypes.Key, groups []receiveAdvertisementGroup, revalidate func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyReceivePolicyRules(cfg); err == nil {
		return nil
	}
	if devices == nil || revalidate == nil || len(groups) == 0 || cfg.routeTable <= unix.RT_TABLE_LOCAL || cfg.fwmark <= 0 {
		return fmt.Errorf("policy recovery requires a complete admitted route projection")
	}
	var pending []netlink.Rule
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
		pending = append(pending, missing...)
	}
	if len(pending) == 0 {
		return nil
	}
	live, err := devices.Device(cfg.iface)
	if err != nil {
		return err
	}
	if key == (wgtypes.Key{}) || live.PublicKey != key.PublicKey() || live.FirewallMark != cfg.fwmark {
		return fmt.Errorf("policy recovery root identity differs")
	}
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return err
	}
	if link.Type() != "wireguard" || link.Attrs().Flags&net.FlagUp == 0 {
		return fmt.Errorf("policy recovery root device is not up")
	}
	owners := map[string]int{}
	for i, group := range groups {
		if group.config.iface != cfg.iface || group.config.routeTable != cfg.routeTable || group.config.fwmark != cfg.fwmark {
			return fmt.Errorf("policy recovery group differs from root scope")
		}
		for _, dst := range group.destinations {
			raw := dst.String()
			if _, exists := owners[raw]; exists {
				return fmt.Errorf("policy recovery destination has multiple owners")
			}
			owners[raw] = i
		}
	}
	installed := make([][]net.IPNet, len(groups))
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		for _, route := range routes {
			if route.Dst == nil || route.Protocol != unix.RTPROT_BOOT {
				return fmt.Errorf("policy recovery table contains an unowned route")
			}
			i, found := owners[route.Dst.String()]
			if !found {
				return fmt.Errorf("policy recovery table contains an unpublished destination")
			}
			installed[i] = append(installed[i], *route.Dst)
		}
	}
	for i, group := range groups {
		if len(installed[i]) == 0 {
			continue
		}
		selected := group.config
		selected.receiveRouteScope = map[string]bool{}
		selected.receiveRoutes = map[string]receiveRouteSelection{}
		selected.receiveRelayRoutes = map[string]string{}
		for _, dst := range installed[i] {
			raw := dst.String()
			selected.receiveRouteScope[raw] = true
			if receiver, ok := group.config.receiveRoutes[raw]; ok {
				selected.receiveRoutes[raw] = receiver
			}
			if relay, ok := group.config.receiveRelayRoutes[raw]; ok {
				selected.receiveRelayRoutes[raw] = relay
			}
		}
		if group.legacy {
			if err := verifyPolicyLegacySources(live, group, installed[i]); err != nil {
				return err
			}
		}
		if err := verifyReceiveRouteDestinations(selected, installed[i], nil, group.own); err != nil {
			return err
		}
	}
	if err := revalidate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Reuse the ordinary idempotent rule writer, including its both-family
	// collision preflight. It never rewrites an existing rule.
	if err := ensureRouteRule(cfg.routeTable, cfg.fwmark); err != nil {
		return err
	}
	return revalidate()
}

func verifyPolicyLegacySources(device *wgtypes.Device, group receiveAdvertisementGroup, destinations []net.IPNet) error {
	var selected *wgtypes.Peer
	for i := range device.Peers {
		if device.Peers[i].PublicKey.String() == group.peer.PublicKey {
			selected = &device.Peers[i]
		}
	}
	if selected == nil || selected.Endpoint == nil || len(selected.AllowedIPs) == 0 {
		return fmt.Errorf("policy recovery lacks an existing peer")
	}
	allowed := map[string]bool{}
	for _, raw := range group.peer.WGAllowedIPs {
		allowed[raw] = true
	}
	for _, prefix := range selected.AllowedIPs {
		if !allowed[prefix.String()] {
			return fmt.Errorf("policy recovery peer has unpublished sources")
		}
	}
	for _, destination := range destinations {
		prefix, err := netip.ParsePrefix(destination.String())
		if err != nil || prefix.Bits() == 0 {
			return fmt.Errorf("policy recovery has invalid destination")
		}
		covered := false
		for _, source := range selected.AllowedIPs {
			p, err := netip.ParsePrefix(source.String())
			if err == nil && p.Addr().BitLen() == prefix.Addr().BitLen() && p.Bits() <= prefix.Bits() && p.Contains(prefix.Addr()) {
				covered = true
			}
		}
		if !covered {
			return fmt.Errorf("policy recovery peer does not cover destination")
		}
		for _, peer := range device.Peers {
			if peer.PublicKey == selected.PublicKey {
				continue
			}
			for _, source := range peer.AllowedIPs {
				p, err := netip.ParsePrefix(source.String())
				if err != nil || p.Overlaps(prefix) {
					return fmt.Errorf("policy recovery destination overlaps another peer")
				}
			}
		}
	}
	return nil
}
