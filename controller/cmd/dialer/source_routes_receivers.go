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
	if err := validateTunnelSourceCandidates(cfg); err != nil {
		return nil, err
	}
	for raw, selected := range cfg.receiveSourceRoutes {
		admitted := false
		for _, candidate := range cfg.receiveSourceCandidates[raw] {
			admitted = admitted || reflect.DeepEqual(selected, candidate)
		}
		if !admitted {
			return nil, fmt.Errorf("source receiver differs from admitted candidates")
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
	return inspectConfiguredTunnelSourcesWithHandshake(cfg, table, legacyIndex, true)
}

// Withdrawal still authenticates keys, permissions and aliases, but cannot
// require a terminated worker to complete a new handshake. Only this read-only
// inspection relaxes liveness; normal source-route installation stays strict.
func inspectConfiguredTunnelSourcesWithHandshake(cfg config, table, legacyIndex int, requireHandshake bool) ([]netlink.Rule, []netlink.Route, error) {
	if err := validateTunnelSourceCandidatesWithLimit(cfg, requireHandshake); err != nil {
		return nil, nil, err
	}
	count := 2
	for _, candidates := range cfg.receiveSourceCandidates {
		if len(candidates) > count {
			count = len(candidates)
		}
	}
	indices := make([]map[string]int, count)
	for i := range indices {
		indices[i] = map[string]int{}
	}
	for raw, candidates := range cfg.receiveSourceCandidates {
		_, dst, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, nil, err
		}
		// Withdrawal authenticates surviving receivers outside its removal scope
		// for preservation only; these candidates never authorize installation.
		if requireHandshake || tunnelSourceInScope(cfg.receiveRouteScope, *dst) {
			if err := validateTunnelSourceScope(cfg, []net.IPNet{*dst}); err != nil {
				return nil, nil, err
			}
		}
		for generation, selection := range candidates {
			link, err := netlink.LinkByName(selection.Interface)
			if isLinkNotFound(err) {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			indices[generation][raw] = link.Attrs().Index
		}
	}
	rules, routes, err := inspectTunnelSourceState(cfg.routeTable, table, legacyIndex, indices...)
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
		found := false
		for generation, byDestination := range indices {
			if byDestination[raw] == route.LinkIndex {
				check.receiveRoutes[raw] = cfg.receiveSourceCandidates[raw][generation]
				found = true
				break
			}
		}
		if !found {
			return nil, nil, fmt.Errorf("source route has no admitted receiver generation")
		}
		hosts = append(hosts, *route.Dst)
	}
	if _, err := validateReceiveRouteSelectionsWithHandshake(check, hosts, nil, requireHandshake); err != nil {
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

// Limit candidates to the two committed transport generations and reject
// contradictory identities before inspecting or modifying any source routes.
func validateTunnelSourceCandidates(cfg config) error {
	return validateTunnelSourceCandidatesWithLimit(cfg, true)
}

func validateTunnelSourceCandidatesWithLimit(cfg config, twoGenerations bool) error {
	if len(cfg.receiveSourceCandidates) > 0 && cfg.receiveRouteScope == nil {
		return fmt.Errorf("source candidates require journal scope")
	}
	identities := map[string]receiveRouteSelection{}
	for _, candidates := range cfg.receiveSourceCandidates {
		if len(candidates) == 0 || (twoGenerations && len(candidates) > 2) {
			return fmt.Errorf("source destination requires one or two receiver candidates")
		}
		seen := map[string]bool{}
		for _, selection := range candidates {
			if selection.Interface == "" || seen[selection.Interface] {
				return fmt.Errorf("source destination has missing or duplicate receiver identity")
			}
			seen[selection.Interface] = true
			if previous, found := identities[selection.Interface]; found && !reflect.DeepEqual(previous, selection) {
				return fmt.Errorf("source candidates disagree on receiver ownership")
			}
			identities[selection.Interface] = selection
		}
	}
	return nil
}

// Inspection has already authenticated the route. Resolve only its admitted
// identity for withdrawal staging; reconciliation revalidates it before writes.
func installedTunnelSourceReceiver(cfg config, route netlink.Route) (receiveRouteSelection, error) {
	for _, selection := range cfg.receiveSourceCandidates[route.Dst.String()] {
		link, err := netlink.LinkByName(selection.Interface)
		if isLinkNotFound(err) {
			continue
		}
		if err != nil {
			return receiveRouteSelection{}, err
		}
		if link.Attrs().Index == route.LinkIndex {
			return selection, nil
		}
	}
	return receiveRouteSelection{}, fmt.Errorf("installed source receiver is no longer admitted")
}
