package tunnel

import (
	"fmt"
	"net/netip"
	"reflect"
	"slices"
)

// PlanReceiveSourceRoutes derives the complete site source-exception selection
// from an admitted journal route set. Legacy journals retain their exceptions;
// receiver-selected journals withdraw them. Allocation identity must agree
// across workers even when their phases differ. Old plans without an explicit
// tunnel projection cannot authorize this operation.
//
// Callers must establish discovery completeness and account for workers outside
// the journal set before replacing a shared source table. This projection alone
// does not authorize deleting unrelated exceptions or executing a phase.
func PlanReceiveSourceRoutes(intent ReceiveJournalRoutes) (map[string]ReceiveSiteTunnel, error) {
	fail := func(reason string) (map[string]ReceiveSiteTunnel, error) {
		return nil, fmt.Errorf("journal source routes: %s", reason)
	}
	var plans, selected []ReceiveTransitionPlan
	for machine, p := range intent.Plans {
		if machine != p.MachineUID {
			return fail("plan identity differs")
		}
		plans = append(plans, p)
		if legacy, found := intent.Legacy[machine]; found {
			if !reflect.DeepEqual(legacy, p) {
				return fail("legacy selection differs")
			}
		} else {
			selected = append(selected, p)
		}
	}
	for machine := range intent.Legacy {
		if _, found := intent.Plans[machine]; !found {
			return fail("legacy selection is not admitted")
		}
	}
	all, err := PlanReceiveRouteSet(plans)
	if err != nil {
		return nil, err
	}
	expected := map[string]map[string]ReceiveRouteSelection{}
	for node := range all {
		expected[node] = map[string]ReceiveRouteSelection{}
	}
	if len(selected) > 0 {
		active, err := PlanReceiveRouteSet(selected)
		if err != nil {
			return nil, err
		}
		for node, routes := range active {
			expected[node] = routes
		}
	}
	if !reflect.DeepEqual(expected, intent.Receivers) {
		return fail("receiver and legacy generation selections disagree")
	}
	result := map[string]ReceiveSiteTunnel{}
	hostSets := map[string]map[string]bool{}
	allocations := map[netip.Addr]string{}
	for _, p := range plans {
		_, legacy := intent.Legacy[p.MachineUID]
		for _, site := range p.Sites {
			if site.Legacy == nil || site.Legacy.Tunnel == nil {
				return fail("explicit site tunnel projection missing")
			}
			tunnel := site.Legacy.Tunnel
			current, found := result[site.NodeUID]
			if found && !slices.Equal(current.Addresses, tunnel.Addresses) {
				return fail("site allocation differs across journals")
			}
			if !found {
				for _, raw := range tunnel.Addresses {
					prefix, err := netip.ParsePrefix(raw)
					if err != nil {
						return nil, err
					}
					if owner, exists := allocations[prefix.Addr()]; exists && owner != site.NodeUID {
						return fail("tunnel allocation belongs to multiple sites")
					}
					allocations[prefix.Addr()] = site.NodeUID
				}
				current = ReceiveSiteTunnel{Addresses: slices.Clone(tunnel.Addresses), Hosts: []string{}}
				hostSets[site.NodeUID] = map[string]bool{}
			}
			if legacy {
				for _, host := range tunnel.Hosts {
					hostSets[site.NodeUID][host] = true
				}
			}
			result[site.NodeUID] = current
		}
	}
	for node, current := range result {
		for host := range hostSets[node] {
			current.Hosts = append(current.Hosts, host)
		}
		slices.Sort(current.Hosts)
		result[node] = current
	}
	return result, nil
}
