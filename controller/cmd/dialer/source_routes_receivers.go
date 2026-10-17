package main

import (
	"fmt"
	"net"
	"reflect"

	"github.com/vishvananda/netlink"
)

// Select source-route devices independently from workload routes. A desired
// receiver must be one of the admitted candidates for that exact destination.
func tunnelSourceReceiverIndices(cfg config, hosts []net.IPNet) (map[string]int, error) {
	if (len(cfg.receiveSourceRoutes) != 0 || len(cfg.receiveSourceCandidates) != 0) && cfg.receiveRouteScope == nil {
		return nil, fmt.Errorf("source receiver selection requires journal scope")
	}
	for raw, selected := range cfg.receiveSourceRoutes {
		candidate, found := cfg.receiveSourceCandidates[raw]
		if !found || !reflect.DeepEqual(selected, candidate) {
			return nil, fmt.Errorf("source receiver differs from admitted candidate")
		}
	}
	check := cfg
	check.receiveRoutes = cfg.receiveSourceRoutes
	return validateReceiveRouteSelections(check, hosts, nil)
}

// Discover only indices of named candidates, then authenticate every candidate
// actually referenced by the inspected table. Missing unused receivers are
// allowed during preparation or after retirement; discovery grants no ownership.
func inspectConfiguredTunnelSources(cfg config, table, legacyIndex int) ([]netlink.Rule, []netlink.Route, error) {
	indices := map[string]int{}
	for raw, selection := range cfg.receiveSourceCandidates {
		_, dst, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, nil, err
		}
		if err := validateTunnelSourceScope(cfg, []net.IPNet{*dst}); err != nil {
			return nil, nil, err
		}
		link, err := netlink.LinkByName(selection.Interface)
		if isLinkNotFound(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		indices[raw] = link.Attrs().Index
	}
	rules, routes, err := inspectTunnelSourceState(cfg.routeTable, table, legacyIndex, indices)
	if err != nil {
		return nil, nil, err
	}
	check := cfg
	check.receiveRoutes = map[string]receiveRouteSelection{}
	var hosts []net.IPNet
	for _, route := range routes {
		if route.LinkIndex == legacyIndex {
			continue
		}
		raw := route.Dst.String()
		check.receiveRoutes[raw] = cfg.receiveSourceCandidates[raw]
		hosts = append(hosts, *route.Dst)
	}
	if _, err := validateReceiveRouteSelections(check, hosts, nil); err != nil {
		return nil, nil, err
	}
	return rules, routes, nil
}

// Include outside-scope devices in final readback only after ownership and
// selectors have been checked. They are preserved, never used for installation.
func preserveUnscopedSourceIndices(cfg config, indices map[string]int, routes []netlink.Route) {
	if cfg.receiveRouteScope == nil {
		return
	}
	for _, route := range routes {
		if !tunnelSourceInScope(cfg.receiveRouteScope, *route.Dst) {
			indices[route.Dst.String()] = route.LinkIndex
		}
	}
}
