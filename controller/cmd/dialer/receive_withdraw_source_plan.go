package main

import (
	"fmt"
	"net"
	"reflect"
	"slices"
	"sort"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// Derive deletion scope from the withdrawn journal, and preservation candidates
// from every admitted journal. Kernel discovery never expands this authority.
func planReceiveSourceWithdrawal(cfg config, plans []tunnel.ReceiveTransitionPlan, root, nodeUID string) (config, []string, []net.IPNet, error) {
	fail := func(err error) (config, []string, []net.IPNet, error) { return config{}, nil, nil, err }
	if root == "" || nodeUID == "" {
		return fail(fmt.Errorf("source withdrawal requires worker and consumer identities"))
	}
	if _, err := tunnel.PlanReceiveRouteSet(plans); err != nil {
		return fail(err)
	}
	cfg.receiveRouteScope = map[string]bool{}
	cfg.receiveSourceRoutes = nil
	cfg.receiveSourceCandidates = map[string][]receiveRouteSelection{}
	hosts := map[string]bool{}
	var addresses []string
	found := false
	for _, plan := range plans {
		graphs := []tunnel.ReceiveTransitionPlan{plan}
		if plan.PreviousReceiver != nil {
			graphs = append(graphs, *plan.PreviousReceiver.Plan)
		}
		for _, graph := range graphs {
			routes, err := tunnel.PlanReceiveRoutes(graph)
			if err != nil {
				return fail(err)
			}
			for _, site := range graph.Sites {
				if site.NodeUID != nodeUID {
					continue
				}
				_, allocated, err := tunnel.ReceiveLocalNetwork(graph, nodeUID)
				if err != nil {
					return fail(err)
				}
				if addresses != nil && !slices.Equal(addresses, allocated) {
					return fail(fmt.Errorf("source withdrawal allocation differs across journals"))
				}
				addresses = allocated
				target := plan.RootPublicKey == root
				if target {
					found = true
					for raw := range routes[nodeUID] {
						cfg.receiveRouteScope[raw] = true
					}
					for _, path := range []*tunnel.ReceiveSitePath{site.Legacy, site.Forward} {
						if path != nil && path.Tunnel != nil {
							for _, raw := range path.Tunnel.Hosts {
								hosts[raw] = true
							}
						}
					}
				}
				if site.Forward == nil || site.Forward.Tunnel == nil {
					continue
				}
				for _, raw := range site.Forward.Tunnel.Hosts {
					selection, ok := routes[nodeUID][raw]
					if !ok {
						return fail(fmt.Errorf("source exception lacks committed receiver"))
					}
					existing := cfg.receiveSourceCandidates[raw]
					duplicate := false
					for _, prior := range existing {
						if prior.Interface == selection.Interface {
							if !reflect.DeepEqual(prior, selection) {
								return fail(fmt.Errorf("source receiver identity differs across journals"))
							}
							duplicate = true
						}
					}
					if !duplicate {
						cfg.receiveSourceCandidates[raw] = append(existing, selection)
					}
				}
			}
		}
	}
	if !found || len(addresses) == 0 || len(cfg.receiveRouteScope) == 0 {
		return fail(fmt.Errorf("withdrawn source owner has no local projection"))
	}
	keys := make([]string, 0, len(hosts))
	for raw := range hosts {
		keys = append(keys, raw)
	}
	sort.Strings(keys)
	admitted := make([]net.IPNet, 0, len(keys))
	for _, raw := range keys {
		_, host, err := net.ParseCIDR(raw)
		if err != nil {
			return fail(err)
		}
		admitted = append(admitted, *host)
	}
	if err := validateTunnelSourceScope(cfg, admitted); err != nil {
		return fail(err)
	}
	if err := validateTunnelSourceCandidatesWithLimit(cfg, false); err != nil {
		return fail(err)
	}
	return cfg, addresses, admitted, nil
}
