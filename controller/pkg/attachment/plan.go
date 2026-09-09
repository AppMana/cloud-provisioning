// Package attachment describes a worker's network placement independently of
// guest OS, distribution bootstrap and cloud API implementation.
package attachment

import (
	"fmt"
	"net/netip"
	"sort"
)

// Machine is a resolved CAPI identity, not an instance name guessed by a guest.
// NetworkID is a provider-qualified network identity (for example an AWS VPC).
type Machine struct {
	UID         string       `json:"uid"`
	NodeUID     string       `json:"nodeUID"`
	ProviderID  string       `json:"providerID"`
	InterfaceID string       `json:"interfaceID"`
	NetworkID   string       `json:"networkID"`
	Subnet      netip.Prefix `json:"subnet"`
	Address     netip.Addr   `json:"address"`
}

// GatewayRequest contains observed transport addresses, never a copy of a
// WireGuard peer's mixed management/transit RouteHosts list.
type GatewayRequest struct {
	Worker        Machine      `json:"worker"`
	Gateway       Machine      `json:"gateway"`
	SiteTransport []netip.Addr `json:"siteTransport"`
	// SiteReturnSources need cloud return routes but retain worker tunnel routes.
	// For example a Linux gateway may emit VXLAN with a mesh source address
	// also used by the worker API balancer. It is not a native CNI destination.
	SiteReturnSources []netip.Addr `json:"siteReturnSources,omitempty"`
	// Underlay lists the outer tunnel endpoint addresses. Return routes to
	// these would send tunnel establishment back through the tunnel itself.
	Underlay []netip.Addr `json:"underlay"`
	UDPPort  uint16       `json:"udpPort"`
	// TCPPorts lists additional control-plane listeners reached through the
	// native gateway, alongside the API port supplied by the runtime. The
	// distribution supplies these (for example k0s Konnectivity on 8132).
	TCPPorts []uint16 `json:"tcpPorts,omitempty"`
}

// GatewayPlan is desired configuration, not evidence that forwarding is ready.
// The infrastructure adapter must resolve the named identities again before
// applying changes and bind route ownership to both Machine UIDs.
type GatewayPlan struct {
	Worker  Machine `json:"worker"`
	Gateway Machine `json:"gateway"`
	// WorkerHost belongs on the gateway's WireGuard peer and on no other peer.
	WorkerHost netip.Prefix `json:"workerHost"`
	// ReturnHosts go through the provider's subnet router to the gateway NIC.
	ReturnHosts []netip.Prefix `json:"returnHosts"`
	// DirectHosts must not be installed as worker tunnel routes: native CNI
	// traffic to these hosts must leave through the physical subnet router.
	DirectHosts []netip.Prefix `json:"directHosts"`
	UDPPort     uint16         `json:"udpPort"`
	TCPPorts    []uint16       `json:"tcpPorts,omitempty"`
}

func validAddress(a netip.Addr) bool {
	return a.IsValid() && a.Zone() == "" && !a.Is4In6() && a.IsGlobalUnicast() && !a.IsLoopback() && !a.IsLinkLocalUnicast()
}

func validateMachine(m Machine) error {
	if m.UID == "" || m.NodeUID == "" || m.ProviderID == "" || m.InterfaceID == "" || m.NetworkID == "" {
		return fmt.Errorf("incomplete CAPI or network identity")
	}
	if !validAddress(m.Address) || !m.Subnet.IsValid() || m.Subnet != m.Subnet.Masked() || !m.Subnet.Contains(m.Address) {
		return fmt.Errorf("native address must belong to a canonical subnet")
	}
	return nil
}

// PlanGateway implements the subnet-Ethernet path observed with native Calico
// VXLAN on Windows 2022/2025. It is also usable for other guests that require
// native Ethernet; the caller supplies the CNI protocol instead of an OS branch.
// The initial capability deliberately requires a shared subnet. Cross-subnet
// routing and IPv6 provider forwarding require their own live qualification.
func PlanGateway(r GatewayRequest) (GatewayPlan, error) {
	if err := validateMachine(r.Worker); err != nil {
		return GatewayPlan{}, fmt.Errorf("worker: %w", err)
	}
	if err := validateMachine(r.Gateway); err != nil {
		return GatewayPlan{}, fmt.Errorf("gateway: %w", err)
	}
	if r.Worker.UID == r.Gateway.UID || r.Worker.NodeUID == r.Gateway.NodeUID || r.Worker.ProviderID == r.Gateway.ProviderID || r.Worker.InterfaceID == r.Gateway.InterfaceID || r.Worker.Address == r.Gateway.Address {
		return GatewayPlan{}, fmt.Errorf("worker and gateway must be distinct machines")
	}
	if r.Worker.NetworkID != r.Gateway.NetworkID || r.Worker.Subnet != r.Gateway.Subnet {
		return GatewayPlan{}, fmt.Errorf("gateway capability requires the worker's network and subnet")
	}
	if !r.Worker.Address.Is4() || r.UDPPort == 0 || len(r.SiteTransport) == 0 || len(r.Underlay) == 0 {
		return GatewayPlan{}, fmt.Errorf("IPv4 native transport, an explicit UDP port, site addresses and outer endpoints are required")
	}
	outer := map[netip.Addr]bool{}
	for _, a := range r.Underlay {
		if !validAddress(a) {
			return GatewayPlan{}, fmt.Errorf("invalid outer tunnel endpoint %s", a)
		}
		outer[a] = true
	}
	if outer[r.Worker.Address] || outer[r.Gateway.Address] {
		return GatewayPlan{}, fmt.Errorf("native attachment address overlaps a tunnel endpoint")
	}
	hosts := map[netip.Addr]bool{}
	direct := map[netip.Addr]bool{}
	for _, a := range r.SiteTransport {
		direct[a] = true
	}
	all := append(append([]netip.Addr(nil), r.SiteTransport...), r.SiteReturnSources...)
	for _, a := range all {
		if !validAddress(a) || !a.Is4() {
			return GatewayPlan{}, fmt.Errorf("invalid IPv4 CNI transport address %s", a)
		}
		if r.Worker.Subnet.Contains(a) || outer[a] {
			return GatewayPlan{}, fmt.Errorf("CNI return route %s would intercept subnet or outer tunnel traffic", a)
		}
		hosts[a] = true
	}
	addresses := make([]netip.Addr, 0, len(hosts))
	for a := range hosts {
		addresses = append(addresses, a)
	}
	sort.Slice(addresses, func(i, j int) bool { return addresses[i].Less(addresses[j]) })
	plan := GatewayPlan{Worker: r.Worker, Gateway: r.Gateway, WorkerHost: netip.PrefixFrom(r.Worker.Address, 32), UDPPort: r.UDPPort}
	ports := map[uint16]bool{}
	for _, port := range r.TCPPorts {
		if port == 0 {
			return GatewayPlan{}, fmt.Errorf("control-plane TCP ports must be nonzero")
		}
		ports[port] = true
	}
	for port := range ports {
		plan.TCPPorts = append(plan.TCPPorts, port)
	}
	sort.Slice(plan.TCPPorts, func(i, j int) bool { return plan.TCPPorts[i] < plan.TCPPorts[j] })
	for _, a := range addresses {
		plan.ReturnHosts = append(plan.ReturnHosts, netip.PrefixFrom(a, 32))
		if direct[a] {
			plan.DirectHosts = append(plan.DirectHosts, netip.PrefixFrom(a, 32))
		}
	}
	return plan, nil
}
