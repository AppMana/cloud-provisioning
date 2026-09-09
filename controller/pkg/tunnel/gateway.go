package tunnel

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

const GatewayProjectionsKey = "gateway-projections.json"

// GatewayProjection is published only after forwarding resources are ready.
// Lease identifies durable attachment ownership; keys bind the two participants.
type GatewayProjection struct {
	Lease         string         `json:"lease"`
	WorkerKey     string         `json:"workerKey"`
	GatewayKey    string         `json:"gatewayKey"`
	WorkerAddress netip.Addr     `json:"workerAddress"`
	WorkerHost    netip.Prefix   `json:"workerHost"`
	DirectHosts   []netip.Prefix `json:"directHosts"`
	// RetiringWorker restores the worker's direct tunnel routes before other
	// consumers withdraw its gateway path. The worker must acknowledge first.
	RetiringWorker bool `json:"retiringWorker,omitempty"`
}

// ProjectGateways applies the shared contract to site and remote peer renders.
// selfKey is empty for site consumers. Gateway consumers use native Ethernet
// to their attached workers and must not install a route through themselves.
func ProjectGateways(data map[string][]byte, peers []PeerSpec, selfKey string) ([]PeerSpec, error) {
	raw := data[GatewayProjectionsKey]
	if len(raw) == 0 {
		return peers, nil
	}
	var plans []GatewayProjection
	if err := json.Unmarshal(raw, &plans); err != nil {
		return nil, fmt.Errorf("gateway projections: %w", err)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].Lease < plans[j].Lease })
	seen := map[string]bool{}
	hosts := map[netip.Addr]bool{}
	for _, p := range plans {
		if p.Lease == "" || seen[p.Lease] || p.WorkerKey == "" || p.GatewayKey == "" || p.WorkerKey == p.GatewayKey || hosts[p.WorkerAddress] || !p.WorkerAddress.Is4() || p.WorkerHost != netip.PrefixFrom(p.WorkerAddress, 32) {
			return nil, fmt.Errorf("invalid or duplicate gateway projection")
		}
		for _, host := range p.DirectHosts {
			if !host.IsValid() || !host.Addr().Is4() || host.Bits() != 32 {
				return nil, fmt.Errorf("invalid direct host")
			}
		}
		seen[p.Lease] = true
		hosts[p.WorkerAddress] = true
	}
	for _, p := range plans {
		if selfKey == p.GatewayKey || (selfKey == p.WorkerKey && p.RetiringWorker) {
			continue
		}
		var err error
		peers, err = GatewayPeers(p, peers, p.GatewayKey, selfKey == p.WorkerKey)
		if err != nil {
			return nil, err
		}
	}
	return peers, nil
}
func remoteSelfKey(data map[string][]byte, address string) string {
	if address == "" {
		return ""
	}
	for key, value := range data {
		if !strings.HasPrefix(key, PeerPublicKeyPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, PeerPublicKeyPrefix)
		hosts := SplitList(string(data[PeerRouteHostsPrefix+name]))
		if len(hosts) == 0 {
			hosts = SplitList(string(data[PeerRouteHostPrefix+name]))
		}
		if containsHost(hosts, address) {
			return strings.TrimSpace(string(value))
		}
	}
	return ""
}
func GatewayPeers(plan GatewayProjection, peers []PeerSpec, gatewayKey string, forWorker bool) ([]PeerSpec, error) {
	if gatewayKey == "" {
		return nil, fmt.Errorf("gateway has not published a WireGuard key")
	}
	if !plan.WorkerHost.IsValid() || plan.WorkerHost.Bits() != 32 || plan.WorkerHost.Addr() != plan.WorkerAddress {
		return nil, fmt.Errorf("invalid gateway worker host")
	}
	target := -1
	for i, p := range peers {
		if p.PublicKey == gatewayKey {
			if target >= 0 {
				return nil, fmt.Errorf("gateway key is published more than once")
			}
			target = i
		}
		for _, raw := range p.WGAllowedIPs {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid peer allowed prefix: %w", err)
			}
			if prefix.Contains(plan.WorkerAddress) && (p.PublicKey != gatewayKey || prefix != plan.WorkerHost) {
				return nil, fmt.Errorf("worker native address is already owned by another peer or a broader prefix")
			}
		}
	}
	if target < 0 {
		return nil, fmt.Errorf("gateway is absent from rendered peers")
	}
	result := make([]PeerSpec, len(peers))
	direct := map[netip.Addr]bool{}
	for _, p := range plan.DirectHosts {
		direct[p.Addr()] = true
	}
	for i, p := range peers {
		p.WGAllowedIPs = append([]string(nil), p.WGAllowedIPs...)
		p.RouteHosts = append([]string(nil), p.RouteHosts...)
		p.Transit = append([]string(nil), p.Transit...)
		p.TransitHosts = append([]string(nil), p.TransitHosts...)
		if forWorker {
			p.RouteHosts = withoutHosts(p.AllRouteHosts(), direct)
			p.RouteHost = ""
			p.TransitHosts = withoutHosts(p.TransitHosts, direct)
		}
		result[i] = p
	}
	if !forWorker {
		p := &result[target]
		p.WGAllowedIPs = appendUnique(p.WGAllowedIPs, plan.WorkerHost.String())
		// Preserve an existing bare or host-prefix spelling without duplicating it.
		if len(withoutHosts(p.AllRouteHosts(), map[netip.Addr]bool{plan.WorkerAddress: true})) == len(p.AllRouteHosts()) {
			p.RouteHosts = append(p.RouteHosts, plan.WorkerAddress.String())
		}
	}
	return result, nil
}

func appendUnique(values []string, value string) []string {
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}

func withoutHosts(values []string, exclude map[netip.Addr]bool) []string {
	var result []string
	for _, raw := range values {
		a, err := netip.ParseAddr(raw)
		if err != nil {
			p, e := netip.ParsePrefix(raw)
			if e == nil && p.Bits() == p.Addr().BitLen() {
				a = p.Addr()
			}
		}
		if !exclude[a] {
			result = append(result, raw)
		}
	}
	return result
}
