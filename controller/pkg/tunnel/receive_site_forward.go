package tunnel

import (
	"fmt"
	"slices"
)

// ValidateReceiveSiteForward prevents a source transfer from also changing the
// admitted workload or tunnel allocation. Only peer ownership and the resulting
// next hop and source exceptions may differ between committed generations.
func ValidateReceiveSiteForward(site ReceiveTransitionSite, public string, desired PeerListDoc, cloud PeerSpec) error {
	if site.Forward == nil {
		return nil
	}
	if site.Legacy == nil || site.Legacy.Tunnel == nil || site.Forward.Tunnel == nil {
		return fmt.Errorf("forward site path requires both allocation projections")
	}
	if !slices.Equal(site.Legacy.Sources, site.Forward.Sources) || !slices.Equal(site.Legacy.Tunnel.Addresses, site.Forward.Tunnel.Addresses) {
		return fmt.Errorf("site allocation changes across forwarding generations")
	}
	if err := ValidateReceiveSiteLegacy(*site.Forward, public, desired, cloud); err != nil {
		return fmt.Errorf("desired site forwarding: %w", err)
	}
	return nil
}
