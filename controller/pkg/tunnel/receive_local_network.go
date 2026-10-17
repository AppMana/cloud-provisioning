package tunnel

import (
	"fmt"
	"net/netip"
	"slices"
)

// ReceiveSiteRouteSources preserves the same preference order as ordinary
// reconciliation. Pod prefixes are cryptokey permissions, not preferred host
// sources, and must not be inferred to be interface addresses.
func ReceiveSiteRouteSources(data map[string][]byte, node string) []string {
	return SplitList(string(data[NodeAddressesPrefix+node]), string(data[SiteAddressesPrefix+node]))
}

// ReceiveLocalNetwork supplies only committed native execution inputs. Older
// journals remain readable, but cannot enter autonomous dispatch without these
// explicit bindings. Cloud route source selection remains kernel-selected.
func ReceiveLocalNetwork(p ReceiveTransitionPlan, nodeUID string) (own, allocated []string, err error) {
	if len(p.CloudAddresses) == 0 {
		return nil, nil, fmt.Errorf("receiver plan lacks cloud allocation")
	}
	if nodeUID == p.CloudNodeUID {
		return nil, slices.Clone(p.CloudAddresses), nil
	}
	for _, site := range p.Sites {
		if site.NodeUID == nodeUID {
			if len(site.RouteSources) == 0 || site.Legacy == nil || site.Legacy.Tunnel == nil {
				return nil, nil, fmt.Errorf("receiver plan lacks site network identity")
			}
			return slices.Clone(site.RouteSources), slices.Clone(site.Legacy.Tunnel.Addresses), nil
		}
	}
	return nil, nil, fmt.Errorf("receiver plan has no local network identity")
}

func validateReceiveLocalNetwork(p ReceiveTransitionPlan) error {
	if p.CloudAddresses == nil {
		for _, site := range p.Sites {
			if site.RouteSources != nil {
				return fmt.Errorf("partial receiver local network projection")
			}
		}
		return nil
	}
	if len(p.CloudAddresses) == 0 {
		return fmt.Errorf("empty receiver cloud allocation")
	}
	families := map[bool]bool{}
	hosts := map[string]bool{}
	for _, peer := range p.SiteDesired.Peers {
		if peer.PublicKey == p.RootPublicKey {
			for _, host := range peer.AllRouteHosts() {
				hosts[HostCIDR(host)] = true
			}
		}
	}
	for _, raw := range p.CloudAddresses {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 || !prefix.Addr().IsGlobalUnicast() || prefix.Addr().Is4In6() || families[prefix.Addr().Is4()] || !hosts[HostCIDR(prefix.Addr().String())] {
			return fmt.Errorf("invalid or unpublished receiver cloud allocation")
		}
		families[prefix.Addr().Is4()] = true
	}
	for _, site := range p.Sites {
		if len(site.RouteSources) == 0 || site.Legacy == nil || site.Forward == nil {
			return fmt.Errorf("missing receiver route source projection")
		}
		seen := map[string]bool{}
		for _, raw := range site.RouteSources {
			address, err := netip.ParseAddr(raw)
			if err != nil || !address.IsGlobalUnicast() || address.Is4In6() || seen[raw] || !slices.Contains(site.Legacy.Sources, HostCIDR(raw)) || !slices.Contains(site.Forward.Sources, HostCIDR(raw)) {
				return fmt.Errorf("receiver route source lacks generation permission")
			}
			seen[raw] = true
		}
	}
	return nil
}
