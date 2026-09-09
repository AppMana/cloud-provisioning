package tunnel

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const SiteAppliedPrefix = "site-applied-"

type SiteApplied struct {
	NodeUID     string `json:"nodeUID"`
	PublicKey   string `json:"publicKey"`
	Hash        string `json:"hash"`
	Role        string `json:"role,omitempty"`
	TransitHash string `json:"transitHash,omitempty"`
}

func SitePeers(data map[string][]byte) ([]PeerSpec, error) { return sitePeers(data, true) }

func sitePeers(data map[string][]byte, strict bool) ([]PeerSpec, error) {
	// A successfully read mesh with no custom workers is authoritative empty
	// state, not an absent/null document. Site endpoints must withdraw the last
	// worker while remaining available for future joins.
	peers := make([]PeerSpec, 0)
	for key, val := range data {
		if !strings.HasPrefix(key, PeerPublicKeyPrefix) {
			continue
		}
		machine := strings.TrimPrefix(key, PeerPublicKeyPrefix)
		endpoint := strings.TrimSpace(string(data[PeerEndpointPrefix+machine]))
		if endpoint == PeerEndpointPending {
			endpoint = ""
		}
		allowedIPsRaw, ok := data[PeerAllowedIPsPrefix+machine]
		if !ok && strict {
			return nil, fmt.Errorf("secret has %s but no matching %s%s", key, PeerAllowedIPsPrefix, machine)
		}
		var routeHosts []string
		if raw, ok := data[PeerRouteHostsPrefix+machine]; ok {
			routeHosts = SplitList(string(raw))
		} else if raw, ok := data[PeerRouteHostPrefix+machine]; ok {
			routeHosts = []string{strings.TrimSpace(string(raw))}
		} else if strict {
			return nil, fmt.Errorf("secret has %s but no matching %s%s", key, PeerRouteHostsPrefix, machine)
		}
		peers = append(peers, PeerSpec{
			PublicKey:    strings.TrimSpace(string(val)),
			Endpoint:     endpoint,
			WGAllowedIPs: SplitList(string(allowedIPsRaw)),
			RouteHosts:   routeHosts,
			// A machine entry is a node somewhere else. The rest of
			// this site cannot reach it without transiting here.
			Remote: true,
		})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PublicKey < peers[j].PublicKey })
	return ProjectGateways(data, peers, "")
}

func SitePeerHash(data map[string][]byte) (string, error) {
	peers, err := SitePeers(data)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(PeerListDoc{Peers: peers})
	if err != nil {
		return "", err
	}
	return HashPeerList(raw), nil
}

// SiteConverged verifies content and current Node/key identity. Missing receipts
// are not convergence; endpoint presence or elapsed time cannot replace one.
func SiteConverged(data map[string][]byte, nodeName, nodeUID string, readiness ...map[string]bool) bool {
	if nodeName == "" || nodeUID == "" {
		return false
	}
	var ack SiteApplied
	if err := json.Unmarshal(data[SiteAppliedPrefix+nodeName], &ack); err != nil {
		return false
	}
	key := strings.TrimSpace(string(data[NodePublicKeyPrefix+nodeName]))
	if ack.NodeUID != nodeUID || ack.PublicKey != key {
		return false
	}
	var notReady map[string]bool
	if len(readiness) > 0 {
		notReady = readiness[0]
	}
	role, transit, err := SiteForwardingRole(data, nodeName, notReady)
	if err != nil {
		return false
	}
	if role != "transit" && key == "" {
		return false
	}
	if role == "relayed" || role == "transit" {
		if ack.Role != role {
			return false
		}
		h, err := SiteTransitHash(data, transit)
		if err != nil || h != ack.TransitHash {
			return false
		}
	} else if ack.Role != "" && ack.Role != "direct" {
		return false
	}
	hash, err := SitePeerHash(data)
	return err == nil && hash == ack.Hash
}

// SiteForwardingRole distinguishes a retained endpoint from the elected relay.
func SiteForwardingRole(data map[string][]byte, node string, notReady map[string]bool) (string, *TransitSpec, error) {
	addresses := SplitList(string(data[SiteAddressesPrefix+node]))
	if len(addresses) == 0 {
		return "direct", nil, nil
	}
	transit, err := SiteTransit(data, notReady)
	if err != nil {
		return "", nil, err
	}
	if transit == nil {
		return "", nil, fmt.Errorf("site relay unavailable")
	}
	if len(data[NodeTunnelAddressPrefix+node]) == 0 {
		return "transit", transit, nil
	}
	if containsHost(addresses, transit.Via) {
		return "direct", nil, nil
	}
	return "relayed", transit, nil
}
func SiteTransitHash(data map[string][]byte, transit *TransitSpec) (string, error) {
	if transit == nil {
		return "", fmt.Errorf("transit observation required")
	}
	peers, err := SitePeerHash(data)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Peers   string
		Transit *TransitSpec
	}{peers, transit})
	if err != nil {
		return "", err
	}
	return HashPeerList(raw), nil
}
