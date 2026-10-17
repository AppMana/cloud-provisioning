package main

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// Read-only verification of the source-exception projection. Scoped calls also
// require outside-scope routes to retain their valid shared selectors. Callers
// must derive allocations and destinations from admitted state, never from the
// observed kernel routes. Missing state is an error, not permission to repair.
func verifyTunnelSourceRoutes(cfg config, localAddresses []string, hosts []net.IPNet) error {
	table, err := tunnelSourceTable(cfg.routeTable)
	if err != nil {
		return err
	}
	if err := validateTunnelSourceScope(cfg, hosts); err != nil {
		return err
	}
	sources, desired, err := planTunnelSourceRoutes(localAddresses, hosts)
	if err != nil {
		return err
	}
	index := 0
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil && !(len(hosts) == 0 && isLinkNotFound(err)) {
		return err
	}
	if err == nil {
		index = link.Attrs().Index
	}
	indices, err := tunnelSourceReceiverIndices(cfg, hosts)
	if err != nil {
		return err
	}
	if cfg.receiveRouteScope != nil {
		rules, routes, err := inspectConfiguredTunnelSources(cfg, table, index)
		if err != nil {
			return err
		}
		preserveUnscopedSourceIndices(cfg, indices, routes)
		if err := preserveUnscopedTunnelSources(cfg, localAddresses, sources, desired, rules, routes); err != nil {
			return err
		}
	}
	return verifyTunnelSourceSelection(cfg.routeTable, table, index, sources, desired, indices)
}

func verifyTunnelSourceSelection(base, table, index int, sources map[int]*net.IPNet, desired map[string]bool, receivers ...map[string]int) error {
	rules, routes, err := inspectTunnelSourceState(base, table, index, receivers...)
	if err != nil {
		return err
	}
	if len(rules) != len(sources) {
		return fmt.Errorf("tunnel source selector set differs from desired state")
	}
	seenFamilies := map[int]bool{}
	for _, rule := range rules {
		expected := sources[rule.Family]
		if expected == nil || rule.Src.String() != expected.String() || seenFamilies[rule.Family] {
			return fmt.Errorf("tunnel source selector differs from allocated address")
		}
		seenFamilies[rule.Family] = true
	}
	if len(routes) != len(desired) {
		return fmt.Errorf("tunnel source destination set differs from desired state")
	}
	seen := map[string]bool{}
	for _, route := range routes {
		raw := route.Dst.String()
		expectedIndex := index
		for _, indices := range receivers {
			if indices[raw] > 0 {
				expectedIndex = indices[raw]
			}
		}
		if route.LinkIndex != expectedIndex {
			return fmt.Errorf("tunnel source receiver differs from selected generation")
		}
		if !desired[raw] || seen[raw] {
			return fmt.Errorf("tunnel source destination differs from desired state")
		}
		seen[raw] = true
	}
	return nil
}
