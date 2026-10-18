package tunnel

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
)

// PlanReceiveNeighborRetirements retains previously admitted ordinary peers
// that are absent from the complete current publication. The result is intent,
// not permission to delete kernel state: native retirement must still match the
// exact device, peer permissions and owned routes. Both journal generations
// remain excluded even after their transitions complete.
func PlanReceiveNeighborRetirements(previous, desired []PeerSpec, plans []ReceiveTransitionPlan) ([]PeerSpec, error) {
	if _, err := PlanReceiveNeighborPeers(desired, plans); err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(desired))
	for _, peer := range desired {
		wanted[peer.PublicKey] = true
	}
	absent := make([]PeerSpec, 0)
	for _, peer := range previous {
		if !wanted[peer.PublicKey] {
			absent = append(absent, peer)
		}
	}
	// Validate every exact possibility, then their combined scope. Repeated
	// keys represent interrupted growth, not permission to accept arbitrary
	// subsets during native retirement.
	merged := make([]PeerSpec, 0)
	result := make([]PeerSpec, 0, len(absent))
	positions := map[string]int{}
	for _, peer := range absent {
		admitted, err := PlanReceiveNeighborPeers([]PeerSpec{peer}, plans)
		if err != nil {
			return nil, err
		}
		if len(admitted) == 0 {
			continue
		}
		peer = admitted[0]
		result = append(result, peer)
		if at, found := positions[peer.PublicKey]; found {
			for _, source := range peer.WGAllowedIPs {
				if !slices.Contains(merged[at].WGAllowedIPs, source) {
					merged[at].WGAllowedIPs = append(merged[at].WGAllowedIPs, source)
				}
			}
			for _, host := range peer.RouteHosts {
				if !slices.Contains(merged[at].RouteHosts, host) {
					merged[at].RouteHosts = append(merged[at].RouteHosts, host)
				}
			}
		} else {
			positions[peer.PublicKey] = len(merged)
			merged = append(merged, peer)
		}
	}
	if _, err := PlanReceiveNeighborPeers(merged, plans); err != nil {
		return nil, err
	}
	return result, nil
}

// PlanReceiveNeighborPeers separates ordinary site peers from journal-owned
// identities and destinations. It does not release a journal or authorize a
// native mutation: callers must admit the complete live inventory, serialize
// with phase execution, and check existing kernel ownership before applying it.
// Both generations remain protected, independently of the selected phase.
func PlanReceiveNeighborPeers(peers []PeerSpec, plans []ReceiveTransitionPlan) ([]PeerSpec, error) {
	if len(plans) == 0 || peers == nil {
		return nil, fmt.Errorf("neighbor planning requires a complete peer document and reserved plans")
	}
	if _, err := PlanReceiveRouteSet(plans); err != nil {
		return nil, err
	}
	reserved := map[string]bool{}
	transport := map[string]bool{}
	var protected []netip.Prefix
	for _, plan := range plans {
		reserved[plan.RootPublicKey] = true
		var base PeerListDoc
		if err := json.Unmarshal(plan.Base, &base); err != nil {
			return nil, err
		}
		for _, document := range []PeerListDoc{base, plan.Desired} {
			for _, peer := range document.Peers {
				if peer.Remote {
					continue
				}
				transport[peer.PublicKey] = true
				prefixes, err := neighborPeerPrefixes(peer)
				if err != nil {
					return nil, err
				}
				protected = append(protected, prefixes...)
			}
		}
		for public, site := range plan.Sites {
			transport[public] = true
			for _, raw := range site.Client.AllowedSources {
				prefix, err := neighborPrefix(raw)
				if err != nil {
					return nil, err
				}
				protected = append(protected, prefix)
			}
			for _, path := range []*ReceiveSitePath{site.Legacy, site.Forward} {
				if path == nil {
					continue
				}
				for _, raw := range path.Sources {
					prefix, err := neighborPrefix(raw)
					if err != nil {
						return nil, err
					}
					protected = append(protected, prefix)
				}
			}
		}
		for _, binding := range plan.Bindings {
			transport[binding.PublicKey] = true
		}
		for _, document := range []PeerListDoc{plan.PreviousSitePeers(), plan.SiteDesired} {
			found := false
			for _, peer := range document.Peers {
				if peer.PublicKey != plan.RootPublicKey {
					continue
				}
				found = true
				prefixes, err := neighborPeerPrefixes(peer)
				if err != nil {
					return nil, err
				}
				protected = append(protected, prefixes...)
			}
			if !found {
				return nil, fmt.Errorf("reserved worker has no destination projection")
			}
		}
	}
	seen := map[string]bool{}
	owners := map[netip.Prefix]string{}
	result := make([]PeerSpec, 0)
	for _, peer := range peers {
		if !validReceivePublicKey(peer.PublicKey) || seen[peer.PublicKey] {
			return nil, fmt.Errorf("invalid or duplicate neighbor identity")
		}
		seen[peer.PublicKey] = true
		if reserved[peer.PublicKey] {
			continue // Only the journal executor may interpret this entry.
		}
		if transport[peer.PublicKey] || !peer.Remote || len(peer.Transit) != 0 || len(peer.TransitHosts) != 0 {
			return nil, fmt.Errorf("neighbor reuses a site or receiver identity")
		}
		prefixes, err := neighborPeerPrefixes(peer)
		if err != nil {
			return nil, err
		}
		for _, prefix := range prefixes {
			for _, owned := range protected {
				if prefix.Overlaps(owned) {
					return nil, fmt.Errorf("neighbor destination overlaps reserved forwarding")
				}
			}
			for owned, public := range owners {
				if public != peer.PublicKey && prefix.Overlaps(owned) {
					return nil, fmt.Errorf("neighbor destinations have competing owners")
				}
			}
			owners[prefix] = peer.PublicKey
		}
		// Return independent slices: preparing ordinary peers must not mutate
		// the controller document or an immutable journal projection.
		peer.WGAllowedIPs = slices.Clone(peer.WGAllowedIPs)
		peer.RouteHosts = slices.Clone(peer.RouteHosts)
		peer.Transit = slices.Clone(peer.Transit)
		peer.TransitHosts = slices.Clone(peer.TransitHosts)
		result = append(result, peer)
	}
	return result, nil
}

func neighborPrefix(raw string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(raw)
	if err != nil || prefix != prefix.Masked() || prefix.Addr().Is4In6() || !prefix.Addr().IsGlobalUnicast() || prefix.Bits() == 0 {
		return netip.Prefix{}, fmt.Errorf("invalid neighbor source prefix %q", raw)
	}
	return prefix, nil
}

func neighborPeerPrefixes(peer PeerSpec) ([]netip.Prefix, error) {
	if len(peer.WGAllowedIPs) == 0 {
		return nil, fmt.Errorf("neighbor has no source authorization")
	}
	var prefixes []netip.Prefix
	for _, raw := range peer.WGAllowedIPs {
		prefix, err := neighborPrefix(raw)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix)
	}
	for _, raw := range peer.AllRouteHosts() {
		address, err := netip.ParseAddr(raw)
		if err != nil || address.Is4In6() || !address.IsGlobalUnicast() {
			return nil, fmt.Errorf("invalid neighbor route host")
		}
		authorized := false
		for _, prefix := range prefixes {
			authorized = authorized || prefix.Contains(address)
		}
		if !authorized {
			return nil, fmt.Errorf("neighbor route exceeds its source authorization")
		}
	}
	return prefixes, nil
}
