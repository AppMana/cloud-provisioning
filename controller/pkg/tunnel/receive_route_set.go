package tunnel

import (
	"fmt"
	"net/netip"
	"reflect"
)

// PlanReceiveRouteSet combines the complete caller-admitted set of active
// receiver plans. It does not decide which phases select receivers, discover
// missing journals, or authorize withdrawals. The caller must preserve legacy
// destinations and supply every active journal before replacing a node's routes.
func PlanReceiveRouteSet(plans []ReceiveTransitionPlan) (map[string]map[string]ReceiveRouteSelection, error) {
	if len(plans) == 0 {
		return nil, fmt.Errorf("receiver route set requires explicit active plans")
	}
	result := map[string]map[string]ReceiveRouteSelection{}
	type identity struct{ name, key, role string }
	nodes := map[string]identity{}
	names := map[string]string{}
	keys := map[string]string{}
	machines := map[string]bool{}
	devices := map[string]map[string]ReceiveRouteSelection{}
	type destination struct {
		prefix  netip.Prefix
		machine string
	}
	destinations := map[string][]destination{}
	first := plans[0]
	bindKey := func(key, node string) error {
		if owner, exists := keys[key]; exists && owner != node {
			return fmt.Errorf("receiver key belongs to conflicting nodes")
		}
		keys[key] = node
		return nil
	}
	bindNode := func(uid, name, key, role string) error {
		if uid == "" || name == "" || key == "" {
			return fmt.Errorf("receiver route set has incomplete node identity")
		}
		id := identity{name, key, role}
		if old, exists := nodes[uid]; exists && old != id {
			return fmt.Errorf("receiver node identity differs across plans")
		}
		if old, exists := names[name]; exists && old != uid {
			return fmt.Errorf("receiver node name refers to multiple incarnations")
		}
		nodes[uid] = id
		names[name] = uid
		return bindKey(key, uid)
	}
	for _, plan := range plans {
		if plan.Namespace == "" || plan.MeshName == "" || plan.MeshUID == "" || plan.Namespace != first.Namespace || plan.MeshName != first.MeshName || plan.MeshUID != first.MeshUID || plan.MachineUID == "" || machines[plan.MachineUID] {
			return nil, fmt.Errorf("receiver route set has inconsistent scope or duplicate machine")
		}
		machines[plan.MachineUID] = true
		// A cloud node cannot be owned by two Machine journals, even if their
		// current destination sets happen not to overlap.
		if _, exists := nodes[plan.CloudNodeUID]; exists {
			return nil, fmt.Errorf("receiver cloud node has multiple journal owners")
		}
		if err := bindNode(plan.CloudNodeUID, plan.CloudNodeName, plan.RootPublicKey, "cloud"); err != nil {
			return nil, err
		}
		routes, err := PlanReceiveRoutes(plan)
		if err != nil {
			return nil, err
		}
		// Previous destinations remain owned during preparation and rollback,
		// even when they are absent from the forward route selection.
		for _, site := range plan.Sites {
			for _, peer := range plan.PreviousSitePeers().Peers {
				if peer.PublicKey != plan.RootPublicKey {
					continue
				}
				oldRoutes := map[string]ReceiveRouteSelection{}
				if err := addReceiveRoutes(oldRoutes, peer, ReceiveRouteSelection{}); err != nil {
					return nil, err
				}
				for raw := range oldRoutes {
					prefix, err := netip.ParsePrefix(raw)
					if err != nil {
						return nil, err
					}
					for _, old := range destinations[site.NodeUID] {
						if old.machine != plan.MachineUID && old.prefix.Overlaps(prefix) {
							return nil, fmt.Errorf("previous journal destinations overlap another owner")
						}
					}
					destinations[site.NodeUID] = append(destinations[site.NodeUID], destination{prefix, plan.MachineUID})
				}
			}
		}
		for key, site := range plan.Sites {
			if err := bindNode(site.NodeUID, site.NodeName, key, "site"); err != nil {
				return nil, err
			}
			if err := bindKey(plan.Bindings[key].PublicKey, plan.CloudNodeUID); err != nil {
				return nil, err
			}
		}
		for node, byDestination := range routes {
			if result[node] == nil {
				result[node] = map[string]ReceiveRouteSelection{}
				devices[node] = map[string]ReceiveRouteSelection{}
			}
			for raw, selection := range byDestination {
				if old, exists := devices[node][selection.Interface]; exists && !reflect.DeepEqual(old, selection) {
					return nil, fmt.Errorf("receiver interface has conflicting identities or permissions")
				}
				devices[node][selection.Interface] = selection
				prefix, err := netip.ParsePrefix(raw)
				if err != nil {
					return nil, err
				}
				for _, old := range destinations[node] {
					if old.machine != plan.MachineUID && old.prefix.Overlaps(prefix) {
						return nil, fmt.Errorf("receiver journals overlap destination ownership on a node")
					}
				}
				destinations[node] = append(destinations[node], destination{prefix, plan.MachineUID})
				result[node][raw] = selection
			}
		}
	}
	return result, nil
}
