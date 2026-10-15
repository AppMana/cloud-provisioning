package tunnel

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// SiteReceiveClient prepares a separate site device for a cloud receiver key.
// The old cloud key can remain on the site's original interface while outbound
// and return routing change independently. No route is part of this plan.
type SiteReceiveClient struct {
	Interface          string   `json:"interface"`
	SitePublicKey      string   `json:"sitePublicKey"`
	CloudRootPublicKey string   `json:"cloudRootPublicKey"`
	PublicKey          string   `json:"publicKey"`
	Endpoint           string   `json:"endpoint"`
	AllowedSources     []string `json:"allowedSources"`
}

// PlanSiteReceiveClient consumes a binding already validated against current
// cloud readiness. Permissions come only from the site's old and desired cloud
// peer documents, never from the binding's claimed source ranges.
func PlanSiteReceiveClient(mesh, site, cloudRoot string, binding ReceiveBinding, previous, desired PeerListDoc) (SiteReceiveClient, error) {
	if strings.TrimSpace(mesh) == "" || !validReceivePublicKey(site) || !validReceivePublicKey(cloudRoot) || site == cloudRoot || binding.PeerPublicKey != site || !validReceivePublicKey(binding.PublicKey) || binding.PublicKey == site || binding.PublicKey == cloudRoot || binding.ListenPort < 1 || binding.ListenPort > 65535 {
		return SiteReceiveClient{}, fmt.Errorf("invalid site receiver identities or binding")
	}
	// Reuse generation ownership checks, including malformed cloud prefixes.
	if _, err := PlanReceiveHandover(mesh, previous, desired, nil); err != nil {
		return SiteReceiveClient{}, err
	}
	sources := map[string]bool{}
	endpoint := ""
	foundDesired := false
	for generation, doc := range []PeerListDoc{previous, desired} {
		for _, peer := range doc.Peers {
			if !peer.Remote {
				return SiteReceiveClient{}, fmt.Errorf("site client input contains a site peer")
			}
			if peer.PublicKey != cloudRoot {
				continue
			}
			for _, prefix := range peer.WGAllowedIPs {
				sources[prefix] = true
			}
			if generation == 1 {
				endpoint = peer.Endpoint
				foundDesired = true
			}
		}
	}
	if !foundDesired || len(sources) == 0 {
		return SiteReceiveClient{}, fmt.Errorf("site client requires a desired cloud peer with source permissions")
	}
	address, err := netip.ParseAddrPort(endpoint)
	if err != nil || address.Port() == 0 || address.Addr().Is4In6() || address.Addr().Zone() != "" || !address.Addr().IsGlobalUnicast() {
		return SiteReceiveClient{}, fmt.Errorf("site client requires an explicit cloud endpoint address")
	}
	sum := sha256.Sum256([]byte(mesh + "\x00" + site + "\x00" + cloudRoot))
	result := SiteReceiveClient{Interface: fmt.Sprintf("cldc%x", sum[:5]), SitePublicKey: site, CloudRootPublicKey: cloudRoot, PublicKey: binding.PublicKey, Endpoint: netip.AddrPortFrom(address.Addr(), uint16(binding.ListenPort)).String(), AllowedSources: make([]string, 0, len(sources))}
	for prefix := range sources {
		result.AllowedSources = append(result.AllowedSources, prefix)
	}
	slices.Sort(result.AllowedSources)
	return result, nil
}
