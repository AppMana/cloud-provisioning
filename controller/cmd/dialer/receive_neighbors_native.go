package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// installReceiveNeighbors applies an admitted ordinary peer partition. The
// caller holds the reconciliation lock and persists/revalidates its complete
// journal inventory and mesh publication before calling. It cannot retire a
// peer, withdraw existing source authorization, or recreate the root device.
func installReceiveNeighbors(ctx context.Context, devices receiverDevices, cfg config, key wgtypes.Key, peers []tunnel.PeerSpec, own []string) error {
	return reconcileReceiveNeighborPartition(ctx, devices, cfg, key, peers, own, false)
}

// Recovery repairs only this independently admitted partition. Shared policy
// rules can remain absent until the full live-mesh policy verifier admits them.
func restoreReceiveNeighbors(ctx context.Context, devices receiverDevices, cfg config, key wgtypes.Key, peers []tunnel.PeerSpec, own []string) error {
	return reconcileReceiveNeighborPartition(ctx, devices, cfg, key, peers, own, true)
}

func reconcileReceiveNeighborPartition(ctx context.Context, devices receiverDevices, cfg config, key wgtypes.Key, peers []tunnel.PeerSpec, own []string, recovery bool) error {
	if len(peers) == 0 {
		return nil
	}
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return err
	}
	if link.Type() != "wireguard" || cfg.routeTable <= unix.RT_TABLE_LOCAL || cfg.fwmark <= 0 {
		return fmt.Errorf("neighbor reconciliation requires the existing policy-routed WireGuard device")
	}
	live, err := devices.Device(cfg.iface)
	if err != nil {
		return err
	}
	if key == (wgtypes.Key{}) || live.PublicKey != key.PublicKey() || live.FirewallMark != cfg.fwmark {
		return fmt.Errorf("neighbor root device identity changed")
	}
	current := map[wgtypes.Key]wgtypes.Peer{}
	for _, peer := range live.Peers {
		current[peer.PublicKey] = peer
	}
	endpoints := map[string]bool{}
	for _, peer := range live.Peers {
		if peer.Endpoint != nil {
			endpoints[peer.Endpoint.IP.String()] = true
		}
	}
	var wanted []wgtypes.PeerConfig
	var destinations []net.IPNet
	selected := map[wgtypes.Key]bool{}
	owners := map[netip.Prefix]wgtypes.Key{}
	keepalive := time.Duration(cfg.keepaliveSecs) * time.Second
	local := localIPs()
	cfg.receiveRouteScope = map[string]bool{}
	cfg.receiveRoutes, cfg.receiveRelayRoutes = nil, nil
	for _, peer := range peers {
		public, err := wgtypes.ParseKey(peer.PublicKey)
		if err != nil || public == key.PublicKey() || selected[public] || !peer.Remote || len(peer.Transit) != 0 || len(peer.TransitHosts) != 0 {
			return fmt.Errorf("invalid neighbor identity or transit projection")
		}
		selected[public] = true
		endpoint, err := net.ResolveUDPAddr("udp", peer.Endpoint)
		if err != nil || endpoint == nil || endpoint.IP == nil || endpoint.Port <= 0 {
			return fmt.Errorf("neighbor requires a resolved endpoint")
		}
		endpoints[endpoint.IP.String()] = true
		var allowed []net.IPNet
		for _, raw := range peer.WGAllowedIPs {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil || prefix != prefix.Masked() || prefix.Bits() == 0 || prefix.Addr().Is4In6() {
				return fmt.Errorf("noncanonical neighbor source policy")
			}
			for other, owner := range owners {
				if other.Overlaps(prefix) && owner != public {
					return fmt.Errorf("neighbor source policies overlap")
				}
			}
			for _, existing := range live.Peers {
				if existing.PublicKey == public {
					continue
				}
				for _, source := range existing.AllowedIPs {
					other, err := netip.ParsePrefix(source.String())
					if err != nil || other.Overlaps(prefix) {
						return fmt.Errorf("neighbor would take another live peer's source authorization")
					}
				}
			}
			parsed, err := parseAllowedIP(raw, local, endpoints, true)
			if err != nil {
				return err
			}
			owners[prefix] = public
			allowed = append(allowed, parsed)
			if !cfg.receiveRouteScope[raw] {
				cfg.receiveRouteScope[raw] = true
				destinations = append(destinations, parsed)
			}
		}
		if len(allowed) == 0 {
			return fmt.Errorf("neighbor has no source authorization")
		}
		if old, found := current[public]; found {
			desired := prefixSet(allowed)
			for raw := range prefixSet(old.AllowedIPs) {
				if !desired[raw] {
					return fmt.Errorf("withdrawing existing neighbor source authorization requires a separate transition")
				}
			}
			// The published endpoint is a bootstrap hint. Reasserting it over
			// an authenticated NAT mapping interrupts an established neighbor
			// until another inbound packet teaches WireGuard the mapping again.
			endpoint = receiveNeighborEndpoint(endpoint, old, time.Now())
		}
		wanted = append(wanted, wgtypes.PeerConfig{PublicKey: public, Endpoint: endpoint, AllowedIPs: allowed, ReplaceAllowedIPs: true, PersistentKeepaliveInterval: &keepalive})
	}
	if err := preflightReceiveRoutePhase(cfg, nil, own); err != nil {
		return err
	}
	// installRoutes migrates owned legacy main-table routes. A new ordinary
	// partition has no authority to adopt such a preexisting route.
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		existing, err := netlink.RouteListFiltered(family, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		for _, route := range existing {
			for raw := range cfg.receiveRouteScope {
				destination, _ := netip.ParsePrefix(raw)
				if (family == netlink.FAMILY_V4) != destination.Addr().Is4() {
					continue
				}
				if route.Dst == nil {
					return fmt.Errorf("neighbor would override a default policy route")
				}
				prefix, err := netip.ParsePrefix(route.Dst.String())
				if err != nil || (!cfg.receiveRouteScope[route.Dst.String()] && prefix.Overlaps(destination)) {
					return fmt.Errorf("neighbor would override overlapping route ownership")
				}
			}
		}
		if err := validateTunnelRuleActions(family, cfg.routeTable, cfg.fwmark); err != nil {
			return err
		}
		rules, err := netlink.RuleList(family)
		if err != nil {
			return err
		}
		missing, err := planTunnelRules(rules, family, cfg.routeTable, cfg.fwmark)
		if err != nil || (!recovery && len(missing) != 0) {
			return fmt.Errorf("neighbor requires intact existing tunnel policy: %v", err)
		}
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: unix.RT_TABLE_MAIN, LinkIndex: link.Attrs().Index}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
		if err != nil {
			return err
		}
		for _, route := range routes {
			if route.Dst != nil && route.Protocol == unix.RTPROT_BOOT && cfg.receiveRouteScope[route.Dst.String()] {
				return fmt.Errorf("neighbor scope has an unadmitted main-table route")
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Delta planning sees only this partition. Giving it the complete device
	// would turn protected peers absent from wanted into removals.
	partition := *live
	partition.Peers = nil
	for _, peer := range live.Peers {
		if selected[peer.PublicKey] {
			partition.Peers = append(partition.Peers, peer)
		}
	}
	if delta, changed := deviceDelta(wgtypes.Config{Peers: wanted}, &partition); changed {
		if err := devices.ConfigureDevice(cfg.iface, delta); err != nil {
			return err
		}
	}
	if err := installRoutesWithPolicy(cfg, destinations, nil, nil, nil, nil, own, !recovery); err != nil {
		return err
	}
	after, err := devices.Device(cfg.iface)
	if err != nil {
		return err
	}
	if after.PublicKey != live.PublicKey || after.FirewallMark != live.FirewallMark || after.ListenPort != live.ListenPort {
		return fmt.Errorf("neighbor installation changed root device identity")
	}
	for _, old := range live.Peers {
		found := false
		for _, peer := range after.Peers {
			if peer.PublicKey == old.PublicKey {
				previous, current := prefixSet(old.AllowedIPs), prefixSet(peer.AllowedIPs)
				found = selected[old.PublicKey] || len(current) == len(previous)
				for raw := range previous {
					found = found && current[raw]
				}
			}
		}
		if !found {
			return fmt.Errorf("neighbor installation changed an existing peer")
		}
	}
	for _, want := range peers {
		found := false
		for _, peer := range after.Peers {
			if peer.PublicKey.String() == want.PublicKey {
				found = receiveSourcesMatch(want.WGAllowedIPs, peer.AllowedIPs)
			}
		}
		if !found {
			return fmt.Errorf("neighbor peer readback differs from admitted policy")
		}
	}
	return ctx.Err()
}

func receiveNeighborEndpoint(configured *net.UDPAddr, live wgtypes.Peer, now time.Time) *net.UDPAddr {
	if live.Endpoint != nil && !live.LastHandshakeTime.IsZero() && !live.LastHandshakeTime.After(now) && now.Sub(live.LastHandshakeTime) <= receiveHandshakeMaxAge {
		// Omit the update, rather than copying an endpoint that may roam again
		// between this read and ConfigureDevice. Keys and sources remain subject
		// to the caller's independent admission and withdrawal checks.
		return nil
	}
	return configured
}
