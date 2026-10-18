package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"sort"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
)

// Recompute the committed projection before using it as mutation authority.
// Stable owners use host masks; source projections retain allocation masks.
func receivePhaseSources(intent tunnel.ReceiveJournalRoutes, node string, allocated []string) ([]string, []net.IPNet, []net.IPNet, error) {
	addresses := allocated
	var desired, admitted []net.IPNet
	projected := intent.Sources != nil
	for _, plan := range intent.Plans {
		for _, site := range plan.Sites {
			projected = projected || (site.Legacy != nil && site.Legacy.Tunnel != nil)
		}
	}
	if !projected {
		return addresses, nil, nil, nil
	}
	expected, err := tunnel.PlanReceiveSourceRoutes(intent)
	if err != nil {
		return nil, nil, nil, err
	}
	if !reflect.DeepEqual(expected, intent.Sources) {
		return nil, nil, nil, fmt.Errorf("native source selection differs from committed journals")
	}
	if local, found := expected[node]; found {
		actual, err := tunnelSourceAllocations(allocated)
		if err != nil {
			return nil, nil, nil, err
		}
		want, err := tunnelSourceAllocations(local.Addresses)
		if err != nil || !reflect.DeepEqual(actual, want) {
			return nil, nil, nil, fmt.Errorf("stable owner and source allocation differ")
		}
		addresses = local.Addresses
		for _, raw := range local.Hosts {
			_, host, err := net.ParseCIDR(raw)
			if err != nil {
				return nil, nil, nil, err
			}
			desired = append(desired, *host)
		}
	}
	seen := map[string]bool{}
	for _, plan := range intent.Plans {
		for _, site := range plan.Sites {
			if site.NodeUID != node {
				continue
			}
			hosts := append([]string(nil), site.Legacy.Tunnel.Hosts...)
			if site.Forward != nil {
				hosts = append(hosts, site.Forward.Tunnel.Hosts...)
			}
			for _, raw := range hosts {
				if seen[raw] {
					continue
				}
				seen[raw] = true
				_, host, err := net.ParseCIDR(raw)
				if err != nil {
					return nil, nil, nil, err
				}
				admitted = append(admitted, *host)
			}
		}
	}
	return addresses, desired, admitted, nil
}

// A retained bare receiver cannot accept workload sources belonging to another
// peer in both generations. Do not activate direct egress until the prepared
// receiver explicitly accepts every local workload and tunnel source.
func validateReceiveSiteSources(p tunnel.ReceiveTransitionPlan, public string) error {
	site := p.Sites[public]
	if site.Legacy == nil || site.Legacy.Tunnel == nil {
		return fmt.Errorf("site source allocation missing")
	}
	var base tunnel.PeerListDoc
	if err := json.Unmarshal(p.Base, &base); err != nil {
		return err
	}
	prepared, err := tunnel.PlanReceiveHandover(p.MeshUID, base, p.Desired, p.Native)
	if err != nil {
		return err
	}
	var allowed []netip.Prefix
	for _, path := range prepared.Receivers {
		if path.PublicKey != public {
			continue
		}
		for _, raw := range path.AllowedSources {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				return err
			}
			allowed = append(allowed, prefix)
		}
	}
	sources := append([]string(nil), site.Legacy.Sources...)
	for _, raw := range site.Legacy.Tunnel.Addresses {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return err
		}
		sources = append(sources, netip.PrefixFrom(prefix.Addr(), prefix.Addr().BitLen()).String())
	}
	for _, raw := range sources {
		source, err := netip.ParsePrefix(raw)
		if err != nil {
			return err
		}
		covered := false
		for _, prefix := range allowed {
			covered = covered || (prefix.Bits() <= source.Bits() && prefix.Contains(source.Addr()))
		}
		if !covered {
			return fmt.Errorf("prepared receiver does not accept allocated site source %s", raw)
		}
	}
	return nil
}

// Permit retries after partial family updates, but only for source destinations
// and selectors bound to the committed plan. Never derive authority from live
// state. Outside-scope policy must already be valid and remains unchanged.
func preflightReceiveSourceSwitch(cfg config, addresses []string, desired, admitted []net.IPNet) ([]net.IPNet, error) {
	hosts, _, err := preflightReceiveSourceSwitchPlan(cfg, addresses, desired, admitted)
	return hosts, err
}

func preflightReceiveSourceSwitchPlan(cfg config, addresses []string, desired, admitted []net.IPNet) ([]net.IPNet, map[string]receiveRouteSelection, error) {
	if cfg.receiveRouteScope == nil {
		return nil, nil, fmt.Errorf("source switch requires journal scope")
	}
	if err := validateTunnelSourceScope(cfg, admitted); err != nil {
		return nil, nil, err
	}
	if err := validateTunnelSourceScope(cfg, desired); err != nil {
		return nil, nil, err
	}
	allocations, err := tunnelSourceAllocations(addresses)
	if err != nil {
		return nil, nil, err
	}
	_, permitted, err := planTunnelSourceRoutes(addresses, admitted)
	if err != nil {
		return nil, nil, err
	}
	sources, wanted, err := planTunnelSourceRoutes(addresses, desired)
	if err != nil {
		return nil, nil, err
	}
	for raw := range wanted {
		if !permitted[raw] {
			return nil, nil, fmt.Errorf("source switch exceeds committed exceptions")
		}
	}
	table, err := tunnelSourceTable(cfg.routeTable)
	if err != nil {
		return nil, nil, err
	}
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return nil, nil, err
	}
	rules, routes, err := inspectConfiguredTunnelSources(cfg, table, link.Attrs().Index)
	if err != nil {
		return nil, nil, err
	}
	if err := preserveUnscopedTunnelSources(cfg, addresses, sources, map[string]bool{}, rules, routes); err != nil {
		return nil, nil, err
	}
	for _, rule := range rules {
		if allocations[rule.Family] == nil || rule.Src.String() != allocations[rule.Family].String() {
			return nil, nil, fmt.Errorf("source switch found another allocation")
		}
	}
	selected := map[string]receiveRouteSelection{}
	for raw, selection := range cfg.receiveSourceRoutes {
		selected[raw] = selection
	}
	for _, route := range routes {
		if !tunnelSourceInScope(cfg.receiveRouteScope, *route.Dst) {
			continue
		}
		raw := route.Dst.String()
		if !permitted[raw] {
			return nil, nil, fmt.Errorf("source switch found an uncommitted destination")
		}
		if !wanted[raw] && route.LinkIndex != link.Attrs().Index {
			receiver, err := installedTunnelSourceReceiver(cfg, route)
			if err != nil {
				return nil, nil, err
			}
			selected[raw] = receiver
		}
		wanted[raw] = true
	}
	var keys []string
	for raw := range wanted {
		keys = append(keys, raw)
	}
	sort.Strings(keys)
	staged := make([]net.IPNet, 0, len(keys))
	for _, raw := range keys {
		_, host, _ := net.ParseCIDR(raw)
		staged = append(staged, *host)
	}
	return staged, selected, nil
}
