package tunnel

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// ReceiveSiteTunnel records the site's allocated tunnel addresses and the exact
// legacy host exceptions. It is separate from workload sources: only a packet
// sourced from an allocated tunnel address may use a retained bare peer.
type ReceiveSiteTunnel struct {
	Addresses []string `json:"addresses"`
	Hosts     []string `json:"hosts"`
}

func planReceiveSiteTunnel(addresses []string, public string, base PeerListDoc, cloud PeerSpec, relayed bool) (ReceiveSiteTunnel, error) {
	fail := func(reason string) (ReceiveSiteTunnel, error) {
		return ReceiveSiteTunnel{}, fmt.Errorf("legacy tunnel exception: %s", reason)
	}
	if len(addresses) == 0 {
		return fail("explicit tunnel allocation required")
	}
	result := ReceiveSiteTunnel{Addresses: make([]string, 0, len(addresses)), Hosts: []string{}}
	subnets := map[bool]netip.Prefix{}
	for _, raw := range addresses {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 || !prefix.Addr().IsGlobalUnicast() || prefix.Addr().Is4In6() {
			return fail("invalid tunnel allocation")
		}
		family := prefix.Addr().Is4()
		if _, exists := subnets[family]; exists {
			return fail("duplicate allocation family")
		}
		subnets[family] = prefix
		result.Addresses = append(result.Addresses, prefix.String())
	}
	slices.Sort(result.Addresses)
	accepted := map[bool]bool{}
	found := false
	for _, peer := range base.Peers {
		if peer.PublicKey != public {
			continue
		}
		if found || peer.Remote {
			return fail("ambiguous legacy peer identity")
		}
		found = true
		for _, raw := range peer.WGAllowedIPs {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil || prefix != prefix.Masked() || prefix.Bits() == 0 || !prefix.Addr().IsGlobalUnicast() || prefix.Addr().Is4In6() {
				return fail("invalid legacy source permission")
			}
			for family, allocation := range subnets {
				if prefix.Contains(allocation.Addr()) {
					accepted[family] = true
				}
			}
		}
	}
	if !relayed {
		for family := range subnets {
			if !accepted[family] {
				return fail("direct legacy peer does not accept allocated tunnel source")
			}
		}
		return result, nil
	}
	seen := map[string]bool{}
	for _, raw := range cloud.AllRouteHosts() {
		host, err := netip.ParsePrefix(HostCIDR(raw))
		if err != nil || host.Bits() != host.Addr().BitLen() || !host.Addr().IsGlobalUnicast() || host.Addr().Is4In6() {
			return fail("invalid cloud route host")
		}
		family := host.Addr().Is4()
		subnet, allocated := subnets[family]
		if !allocated || !accepted[family] || !subnet.Contains(host.Addr()) {
			continue
		}
		if !seen[host.String()] {
			seen[host.String()] = true
			result.Hosts = append(result.Hosts, host.String())
		}
	}
	slices.Sort(result.Hosts)
	return result, nil
}

// ValidateReceiveSiteTunnelAllocation binds a persisted projection to the live
// mesh allocation without re-electing the legacy relay or deriving kernel state.
func ValidateReceiveSiteTunnelAllocation(path ReceiveSiteTunnel, data map[string][]byte, node string) error {
	var actual []string
	for _, field := range []string{NodeTunnelAddressPrefix, NodeTunnelAddress6Prefix} {
		raw := strings.TrimSpace(string(data[field+node]))
		if raw == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return err
		}
		actual = append(actual, prefix.String())
	}
	slices.Sort(actual)
	if len(actual) == 0 || !slices.Equal(actual, path.Addresses) {
		return fmt.Errorf("site tunnel allocation changed after journal creation")
	}
	return nil
}
