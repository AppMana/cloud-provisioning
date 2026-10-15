package tunnel

import (
	"encoding/json"
	"fmt"
	"net/netip"
)

const PeerNativeNetworkPrefix = "peer-native-network-"

// NativeNetwork is published only after provider identity and allocations have
// been verified. Matching network identities permit exact native destinations
// to use the provider's underlay instead of a remote-to-remote tunnel.
type NativeNetwork struct {
	PublicKey string   `json:"publicKey"`
	ID        string   `json:"id"`
	Prefixes  []string `json:"prefixes"`
}

func nativeNetwork(data map[string][]byte, name string) (NativeNetwork, error) {
	var network NativeNetwork
	raw := data[PeerNativeNetworkPrefix+name]
	if len(raw) == 0 {
		return network, nil
	}
	if err := json.Unmarshal(raw, &network); err != nil {
		return network, err
	}
	if network.PublicKey == "" || network.PublicKey != string(data[PeerPublicKeyPrefix+name]) {
		return network, fmt.Errorf("native network peer identity changed")
	}
	if network.ID == "" {
		return network, fmt.Errorf("native network identity missing")
	}
	for _, raw := range network.Prefixes {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix != prefix.Masked() || prefix.Bits() == 0 || prefix.Addr().Is4In6() || prefix.Addr().IsUnspecified() || prefix.Addr().IsLoopback() || prefix.Addr().IsMulticast() {
			return network, fmt.Errorf("invalid native destination %q", raw)
		}
	}
	return network, nil
}

// Site peers keep every provider allocation. Only another verified member of
// the same native network omits its underlay destinations from tunnel routes.
func projectNativeNetwork(data map[string][]byte, self, other string, peer PeerSpec) (PeerSpec, error) {
	local, err := nativeNetwork(data, self)
	if err != nil {
		return peer, err
	}
	remote, err := nativeNetwork(data, other)
	if err != nil {
		return peer, err
	}
	if local.ID == "" || local.ID != remote.ID {
		return peer, nil
	}
	native := map[string]bool{}
	for _, prefix := range remote.Prefixes {
		native[prefix] = true
	}
	allowed := make([]string, 0, len(peer.WGAllowedIPs))
	for _, prefix := range peer.WGAllowedIPs {
		if !native[prefix] {
			allowed = append(allowed, prefix)
		}
	}
	hosts := make([]string, 0, len(peer.RouteHosts))
	for _, host := range peer.AllRouteHosts() {
		prefix := host
		if address, err := netip.ParseAddr(host); err == nil {
			prefix = netip.PrefixFrom(address, address.BitLen()).String()
		}
		if !native[prefix] {
			hosts = append(hosts, host)
		}
	}
	peer.WGAllowedIPs, peer.RouteHosts, peer.RouteHost = allowed, hosts, ""
	return peer, nil
}
