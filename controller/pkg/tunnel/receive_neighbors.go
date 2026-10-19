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
	return planReceiveNeighborRetirements(previous, desired, plans, "")
}

// PlanReceiveCloudNeighborRetirements withdraws only recorded direct cloud
// edges; it grants no authority over either endpoint's site-facing receivers.
func PlanReceiveCloudNeighborRetirements(previous, desired []PeerSpec, plans []ReceiveTransitionPlan, root string) ([]PeerSpec, error) {
	if root == "" {
		return nil, fmt.Errorf("cloud neighbor root is required")
	}
	return planReceiveNeighborRetirements(previous, desired, plans, root)
}

func planReceiveNeighborRetirements(previous, desired []PeerSpec, plans []ReceiveTransitionPlan, cloudRoot string) ([]PeerSpec, error) {
	if _, err := planReceiveNeighborPeers(desired, plans, cloudRoot); err != nil {
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
	// Historical attempts may reuse destinations without ever coexisting in
	// the kernel. Validate each exact intent against protected journal state;
	// native retirement must verify actual owners before changing anything.
	result := make([]PeerSpec, 0, len(absent))
	for _, peer := range absent {
		admitted, err := planReceiveNeighborPeers([]PeerSpec{peer}, plans, cloudRoot)
		if err != nil {
			return nil, err
		}
		result = append(result, admitted...)
	}
	return result, nil
}

// PlanReceiveNeighborPeers separates ordinary site peers from journal-owned
// identities and destinations. It does not release a journal or authorize a
// native mutation: callers must admit the complete live inventory, serialize
// with phase execution, and check existing kernel ownership before applying it.
// Both generations remain protected, independently of the selected phase.
func PlanReceiveNeighborPeers(peers []PeerSpec, plans []ReceiveTransitionPlan) ([]PeerSpec, error) {
	return planReceiveNeighborPeers(peers, plans, "")
}

// PlanReceiveCloudNeighborPeers admits direct cloud edges independently of
// site-facing receivers. Every journal still protects its transport identities
// and destinations; only each remote's committed root may own its direct edge.
func PlanReceiveCloudNeighborPeers(peers []PeerSpec, plans []ReceiveTransitionPlan, root string) ([]PeerSpec, error) {
	if root == "" {
		return nil, fmt.Errorf("cloud neighbor root is required")
	}
	return planReceiveNeighborPeers(peers, plans, root)
}

// PlanReceiveWithdrawnNeighborPeers selects only the withdrawn root's recorded
// ordinary-peer variants. The caller must independently admit persisted
// withdrawal authority against the complete live inventory under the host lock.
// This does not release any reservation or authorize receiver-device cleanup.
func PlanReceiveWithdrawnNeighborPeers(previous, current []PeerSpec, plans []ReceiveTransitionPlan, root string) ([]PeerSpec, error) {
	if !validReceivePublicKey(root) {
		return nil, fmt.Errorf("withdrawn neighbor requires exact root identity")
	}
	if _, err := PlanReceiveNeighborPeers(current, plans); err != nil {
		return nil, err
	}
	found := false
	for _, plan := range plans {
		found = found || plan.RootPublicKey == root
	}
	if !found {
		return nil, fmt.Errorf("withdrawn neighbor has no reserved journal")
	}
	for _, peer := range current {
		if peer.PublicKey == root {
			return nil, fmt.Errorf("withdrawn neighbor remains published")
		}
	}
	result := make([]PeerSpec, 0)
	for _, peer := range previous {
		if peer.PublicKey != root {
			continue
		}
		admitted, err := planReceiveNeighborPeersWithWithdrawal([]PeerSpec{peer}, plans, "", root)
		if err != nil {
			return nil, err
		}
		// Historical variants need not coexist. Current published owners, however,
		// must not overlap any route or permission this operation could remove.
		prefixes, err := neighborPeerPrefixes(peer)
		if err != nil {
			return nil, err
		}
		for _, live := range current {
			other, err := neighborPeerPrefixes(live)
			if err != nil {
				return nil, err
			}
			for _, prefix := range prefixes {
				for _, owned := range other {
					if prefix.Overlaps(owned) {
						return nil, fmt.Errorf("withdrawn neighbor overlaps a published owner")
					}
				}
			}
		}
		result = append(result, admitted...)
	}
	return result, nil
}

func planReceiveNeighborPeers(peers []PeerSpec, plans []ReceiveTransitionPlan, cloudRoot string) ([]PeerSpec, error) {
	return planReceiveNeighborPeersWithWithdrawal(peers, plans, cloudRoot, "")
}

func planReceiveNeighborPeersWithWithdrawal(peers []PeerSpec, plans []ReceiveTransitionPlan, cloudRoot, withdrawnRoot string) ([]PeerSpec, error) {
	if plans == nil || peers == nil {
		return nil, fmt.Errorf("neighbor planning requires a complete peer document and explicit plan inventory")
	}
	// An explicitly empty inventory can follow verified retirement of the last
	// receiver. Nil still means undiscovered authority. The caller must admit
	// live inventory and retained retirement fences before native execution.
	if len(plans) != 0 {
		if _, err := PlanReceiveRouteSet(plans); err != nil {
			return nil, err
		}
	}
	reserved := map[string]bool{}
	transport := map[string]bool{}
	var protected []netip.Prefix
	cloudOwned := map[string][]netip.Prefix{}
	foundCloud := cloudRoot == ""
	for _, plan := range plans {
		if (cloudRoot == "" || cloudRoot == plan.RootPublicKey) && withdrawnRoot != plan.RootPublicKey {
			reserved[plan.RootPublicKey] = true
		}
		foundCloud = foundCloud || cloudRoot == plan.RootPublicKey
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
				if (cloudRoot != "" && cloudRoot != plan.RootPublicKey) || withdrawnRoot == plan.RootPublicKey {
					cloudOwned[plan.RootPublicKey] = append(cloudOwned[plan.RootPublicKey], prefix)
				} else {
					protected = append(protected, prefix)
				}
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
				if (cloudRoot != "" && cloudRoot != plan.RootPublicKey) || withdrawnRoot == plan.RootPublicKey {
					cloudOwned[plan.RootPublicKey] = append(cloudOwned[plan.RootPublicKey], prefixes...)
				} else {
					protected = append(protected, prefixes...)
				}
			}
			if !found {
				return nil, fmt.Errorf("reserved worker has no destination projection")
			}
		}
	}
	if !foundCloud {
		return nil, fmt.Errorf("cloud neighbor root has no reserved journal")
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
			for owner, owned := range cloudOwned {
				for _, other := range owned {
					if owner != peer.PublicKey && prefix.Overlaps(other) {
						return nil, fmt.Errorf("neighbor destination overlaps another cloud owner")
					}
				}
			}
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
