package tunnel

import (
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"
)

// ReceiveSiteLegacy binds workload sources to the old cloud-side cryptokey
// owner. Transit is nil for a direct path and contains only this worker's
// destinations; other workers may still use different legacy relays.
type ReceiveSiteLegacy struct {
	Sources       []string     `json:"sources"`
	PeerPublicKey string       `json:"peerPublicKey"`
	Transit       *TransitSpec `json:"transit,omitempty"`
}

// PlanReceiveSiteLegacy uses the acknowledged cloud document, not the current
// relay election. The caller must bind base to the worker's applied hash.
func PlanReceiveSiteLegacy(data map[string][]byte, node, public, cloud string, base PeerListDoc) (ReceiveSiteLegacy, error) {
	fail := func(reason string) (ReceiveSiteLegacy, error) {
		return ReceiveSiteLegacy{}, fmt.Errorf("legacy site path: %s", reason)
	}
	if node == "" || !validReceivePublicKey(public) || strings.TrimSpace(string(data[NodePublicKeyPrefix+node])) != public {
		return fail("site identity differs")
	}
	sources := map[string]bool{}
	for _, prefix := range []string{NodeAddressesPrefix, SiteAddressesPrefix} {
		for _, raw := range SplitList(string(data[prefix+node])) {
			sources[HostCIDR(raw)] = true
		}
	}
	for _, prefix := range []string{NodePodCIDRsPrefix, SitePodCIDRsPrefix} {
		for _, raw := range SplitList(string(data[prefix+node])) {
			sources[raw] = true
		}
	}
	result := ReceiveSiteLegacy{}
	for raw := range sources {
		result.Sources = append(result.Sources, raw)
	}
	slices.Sort(result.Sources)
	owner, err := receiveLegacySourceOwner(base, result.Sources)
	if err != nil {
		return fail(err.Error())
	}
	result.PeerPublicKey = owner.PublicKey
	peers, err := SitePeers(data)
	if err != nil {
		return fail(err.Error())
	}
	var worker []PeerSpec
	for _, peer := range peers {
		if peer.PublicKey == cloud {
			worker = append(worker, peer)
		}
	}
	if len(worker) != 1 {
		return fail("cloud destination identity missing or ambiguous")
	}
	if owner.PublicKey != public {
		relay := ""
		for key, value := range data {
			if strings.HasPrefix(key, NodePublicKeyPrefix) && strings.TrimSpace(string(value)) == owner.PublicKey {
				if relay != "" {
					return fail("relay identity ambiguous")
				}
				relay = strings.TrimPrefix(key, NodePublicKeyPrefix)
			}
		}
		if relay == "" {
			return fail("legacy relay identity missing")
		}
		result.Transit = siteTransitThrough(data, relay, worker)
		if result.Transit == nil {
			return fail("legacy relay has no next hop")
		}
	}
	if err := ValidateReceiveSiteLegacy(result, public, base, worker[0]); err != nil {
		return fail(err.Error())
	}
	return result, nil
}

// ValidateReceiveSiteLegacy checks the immutable projection at journal admission.
// Source allocation itself is admitted by the controller's mesh snapshot.
func ValidateReceiveSiteLegacy(path ReceiveSiteLegacy, public string, base PeerListDoc, cloud PeerSpec) error {
	owner, err := receiveLegacySourceOwner(base, path.Sources)
	if err != nil {
		return err
	}
	if owner.PublicKey != path.PeerPublicKey {
		return fmt.Errorf("legacy source owner differs")
	}
	if owner.PublicKey == public {
		if path.Transit != nil {
			return fmt.Errorf("direct legacy source owner has a relay")
		}
		return nil
	}
	if path.Transit == nil {
		return fmt.Errorf("legacy relay path missing")
	}
	// Relayed hosts in RouteHosts cannot themselves become next hops.
	ownHosts := map[string]bool{}
	for _, raw := range owner.AllRouteHosts() {
		ownHosts[HostCIDR(raw)] = true
	}
	for _, raw := range owner.TransitHosts {
		delete(ownHosts, HostCIDR(raw))
	}
	for _, raw := range []string{path.Transit.Via, path.Transit.ViaOtherFamily} {
		if raw == "" {
			continue
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil || !addr.IsGlobalUnicast() || addr.Is4In6() || !ownHosts[HostCIDR(raw)] {
			return fmt.Errorf("legacy relay gateway is not its own admitted host")
		}
		gatewayOwner, err := receiveLegacySourceOwner(base, []string{HostCIDR(raw)})
		if err != nil || gatewayOwner.PublicKey != owner.PublicKey {
			return fmt.Errorf("legacy relay gateway is not accepted by its peer")
		}
	}
	if path.Transit.Via == "" {
		return fmt.Errorf("legacy relay gateway missing")
	}
	if path.Transit.ViaOtherFamily != "" {
		a, _ := netip.ParseAddr(path.Transit.Via)
		b, _ := netip.ParseAddr(path.Transit.ViaOtherFamily)
		if a.Is4() == b.Is4() {
			return fmt.Errorf("legacy relay gateways have the same family")
		}
	}
	expected := &TransitSpec{Via: path.Transit.Via, ViaOtherFamily: path.Transit.ViaOtherFamily}
	setTransitDestinations(expected, []PeerSpec{cloud})
	if !reflect.DeepEqual(expected, path.Transit) {
		return fmt.Errorf("legacy relay destinations differ from worker")
	}
	for _, raw := range append(slices.Clone(cloud.WGAllowedIPs), cloud.AllRouteHosts()...) {
		prefix, err := netip.ParsePrefix(HostCIDR(raw))
		if err != nil || path.Transit.Gateway(prefix.Addr().AsSlice()) == nil {
			return fmt.Errorf("legacy relay lacks a destination address family")
		}
	}
	return nil
}

func receiveLegacySourceOwner(base PeerListDoc, sources []string) (PeerSpec, error) {
	if len(sources) == 0 {
		return PeerSpec{}, fmt.Errorf("legacy site source allocation missing")
	}
	seen := map[string]bool{}
	var result PeerSpec
	for _, raw := range sources {
		source, err := netip.ParsePrefix(raw)
		if err != nil || source != source.Masked() || source.Bits() == 0 || !source.Addr().IsGlobalUnicast() || source.Addr().Is4In6() || seen[raw] {
			return PeerSpec{}, fmt.Errorf("invalid legacy site source allocation")
		}
		seen[raw] = true
		owner := ""
		covered := false
		for _, peer := range base.Peers {
			for _, allowed := range peer.WGAllowedIPs {
				prefix, err := netip.ParsePrefix(allowed)
				if err != nil {
					return PeerSpec{}, err
				}
				if !prefix.Overlaps(source) {
					continue
				}
				if peer.Remote || (owner != "" && owner != peer.PublicKey) {
					return PeerSpec{}, fmt.Errorf("ambiguous or partial legacy site source owner")
				}
				owner = peer.PublicKey
				covered = covered || prefix.Bits() <= source.Bits()
				if result.PublicKey != "" && result.PublicKey != owner {
					return PeerSpec{}, fmt.Errorf("legacy site sources use different owners")
				}
				result = peer
			}
		}
		if owner == "" || !covered {
			return PeerSpec{}, fmt.Errorf("legacy cloud document does not accept site source")
		}
	}
	if !validReceivePublicKey(result.PublicKey) {
		return PeerSpec{}, fmt.Errorf("invalid legacy site owner identity")
	}
	return result, nil
}
