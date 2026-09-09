package tunnelhost

import (
	"errors"
	"fmt"
	"net/netip"
	"os"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

func validateBackends(backends []string) error {
	for _, raw := range backends {
		address, err := netip.ParseAddrPort(raw)
		if err != nil || address.Port() == 0 || address.Addr().IsUnspecified() || address.Addr().IsMulticast() || address.Addr().Is4In6() {
			return fmt.Errorf("invalid numeric API backend")
		}
	}
	return nil
}

// APIBackends reads only the host's validated public cache. The proxy never
// races the tunnel owner to consume unpublished updates. Before adoption, or
// for old public documents without API endpoints, use bootstrap endpoints.
// A corrupt existing cache is an error: callers retain their serving list.
func APIBackends(bootstrap tunnel.PeersFileDoc, cache string) ([]string, error) {
	backends := bootstrap.APIServers
	public, err := readPublic(cache)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil && len(public.APIServers) > 0 {
		backends = public.APIServers
	}
	if err := validateBackends(backends); err != nil {
		return nil, err
	}
	if len(backends) == 0 {
		return nil, fmt.Errorf("no API backends available")
	}
	return append([]string(nil), backends...), nil
}
