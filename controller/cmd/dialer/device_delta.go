package main

import (
	"net"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// deviceDelta avoids removing live AllowedIPs or flushing endpoint route
// caches when desired kernel state is unchanged. Prefix additions can be
// appended; withdrawals require replacement of that peer's accept list.
func deviceDelta(want wgtypes.Config, live *wgtypes.Device) (wgtypes.Config, bool) {
	if want.PrivateKey != nil && *want.PrivateKey == live.PrivateKey {
		want.PrivateKey = nil
	}
	if want.ListenPort != nil && *want.ListenPort == live.ListenPort {
		want.ListenPort = nil
	}
	if want.FirewallMark != nil && *want.FirewallMark == live.FirewallMark {
		want.FirewallMark = nil
	}
	current := make(map[wgtypes.Key]wgtypes.Peer, len(live.Peers))
	for _, peer := range live.Peers {
		current[peer.PublicKey] = peer
	}
	peers := make([]wgtypes.PeerConfig, 0, len(want.Peers))
	for _, peer := range want.Peers {
		old, found := current[peer.PublicKey]
		delete(current, peer.PublicKey)
		if !found {
			peers = append(peers, peer)
			continue
		}
		if peer.Endpoint != nil && old.Endpoint != nil && peer.Endpoint.IP.Equal(old.Endpoint.IP) && peer.Endpoint.Port == old.Endpoint.Port && peer.Endpoint.Zone == old.Endpoint.Zone {
			peer.Endpoint = nil
		}
		if peer.PersistentKeepaliveInterval != nil && *peer.PersistentKeepaliveInterval == old.PersistentKeepaliveInterval {
			peer.PersistentKeepaliveInterval = nil
		}
		if peer.ReplaceAllowedIPs {
			previous, desired := prefixSet(old.AllowedIPs), prefixSet(peer.AllowedIPs)
			withdrawal := false
			for prefix := range previous {
				if !desired[prefix] {
					withdrawal = true
					break
				}
			}
			if !withdrawal {
				added := make([]net.IPNet, 0)
				for _, prefix := range peer.AllowedIPs {
					key := canonicalPrefix(prefix)
					if !previous[key] {
						added = append(added, prefix)
						previous[key] = true
					}
				}
				peer.AllowedIPs, peer.ReplaceAllowedIPs = added, false
			}
		}
		if peer.Remove || peer.PresharedKey != nil || peer.Endpoint != nil || peer.PersistentKeepaliveInterval != nil || peer.ReplaceAllowedIPs || len(peer.AllowedIPs) > 0 {
			peers = append(peers, peer)
		}
	}
	// Preserve kernel order for removals so the plan is deterministic.
	for _, peer := range live.Peers {
		if _, remove := current[peer.PublicKey]; remove {
			peers = append(peers, wgtypes.PeerConfig{PublicKey: peer.PublicKey, Remove: true})
		}
	}
	want.Peers = peers
	return want, want.PrivateKey != nil || want.ListenPort != nil || want.FirewallMark != nil || len(peers) > 0
}

func canonicalPrefix(prefix net.IPNet) string {
	return (&net.IPNet{IP: prefix.IP.Mask(prefix.Mask), Mask: prefix.Mask}).String()
}

func prefixSet(prefixes []net.IPNet) map[string]bool {
	set := make(map[string]bool, len(prefixes))
	for _, prefix := range prefixes {
		set[canonicalPrefix(prefix)] = true
	}
	return set
}
