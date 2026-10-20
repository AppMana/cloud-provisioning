package main

import (
	"encoding/json"
	"fmt"
	"net/netip"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// Plan only the departed root's historically published relay destinations.
// Caller must admit the retained follower and persist retirement before native
// mutation. The cached election is evidence of the installed next hop, not
// permission to elect a replacement or replay unrelated forwarding state.
func planWithdrawnTransitRoutes(cache *withdrawnForwardingCache, plans []tunnel.ReceiveTransitionPlan, root string) (map[string]string, error) {
	if cache == nil || !cache.next.Transit {
		return nil, fmt.Errorf("follower withdrawal requires recorded transit forwarding")
	}
	var previous siteForwardingCache
	if err := json.Unmarshal(cache.raw, &previous); err != nil {
		return nil, err
	}
	current, err := tunnel.SitePeers(cache.next.Data)
	if err != nil {
		return nil, err
	}
	recorded := withdrawnTransitPeerVariants(cache, plans, root)
	selected, err := tunnel.PlanReceiveWithdrawnNeighborPeers(recorded, current, plans, root)
	if err != nil {
		return nil, err
	}
	transit, err := tunnel.SiteTransit(previous.Data, previous.NotReady)
	if err != nil {
		return nil, err
	}
	if err := validateSiteTransit(transit); err != nil {
		return nil, err
	}
	if transit == nil {
		for _, peer := range cache.peers {
			if peer.PublicKey == root {
				return nil, fmt.Errorf("recorded follower worker lost its relay evidence")
			}
		}
		// A fresh phase-aware follower may have no legacy worker routes at all.
		// Its per-worker record supplies native ownership; the retirement caller
		// must independently verify absence over the committed destination set.
		return map[string]string{}, nil
	}
	next, err := tunnel.SiteTransit(cache.next.Data, cache.next.NotReady)
	if err != nil {
		return nil, err
	}
	if transit == nil || next == nil || transit.Via != next.Via || transit.ViaOtherFamily != next.ViaOtherFamily {
		return nil, fmt.Errorf("follower withdrawal lacks a stable recorded relay")
	}
	routes := map[string]string{}
	for _, peer := range selected {
		prefixes := append([]string{}, peer.WGAllowedIPs...)
		for _, host := range peer.AllRouteHosts() {
			prefixes = append(prefixes, tunnel.HostCIDR(host))
		}
		for _, raw := range prefixes {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil || prefix != prefix.Masked() || prefix.Bits() == 0 || !prefix.Addr().IsGlobalUnicast() || prefix.Addr().Is4In6() {
				return nil, fmt.Errorf("invalid follower withdrawal destination")
			}
			via := transit.Gateway(prefix.Addr().AsSlice())
			if via == nil {
				return nil, fmt.Errorf("recorded relay lacks withdrawal address family")
			}
			routes[prefix.String()] = via.String()
		}
	}
	return routes, nil
}

func withdrawnTransitPeerVariants(cache *withdrawnForwardingCache, plans []tunnel.ReceiveTransitionPlan, root string) []tunnel.PeerSpec {
	recorded := append([]tunnel.PeerSpec{}, cache.peers...)
	for _, plan := range plans {
		if plan.RootPublicKey != root {
			continue
		}
		recorded = append(recorded, plan.SiteDesired.Peers...)
		if plan.PreviousReceiver != nil {
			recorded = append(recorded, plan.PreviousReceiver.Plan.SiteDesired.Peers...)
		}
	}
	return recorded
}
