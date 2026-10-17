package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func selectedReceiveLegacyPeers(key wgtypes.Key, nodeUID string, intent tunnel.ReceiveJournalRoutes) ([]tunnel.PeerSpec, error) {
	var ids []string
	for uid := range intent.Legacy {
		ids = append(ids, uid)
	}
	slices.Sort(ids)
	seen := map[string]tunnel.PeerSpec{}
	var peers []tunnel.PeerSpec
	for _, uid := range ids {
		plan := intent.Legacy[uid]
		view, found := intent.Views[uid]
		if !found || plan.MachineUID != uid || !reflect.DeepEqual(plan, intent.Plans[uid]) {
			return nil, fmt.Errorf("legacy recovery lacks exact admitted selection")
		}
		generation := view.ServingGeneration
		if !view.Complete {
			generation = view.Round.ToGeneration
			if view.Round.Phase == handover.Prepare || view.Round.Phase == handover.Abort {
				generation = view.Round.FromGeneration
			}
		}
		if generation != "legacy:"+tunnel.HashPeerList(plan.Base) {
			return nil, fmt.Errorf("legacy recovery generation differs")
		}
		var local []tunnel.PeerSpec
		if plan.CloudNodeUID == nodeUID {
			if plan.RootPublicKey != key.PublicKey().String() {
				return nil, fmt.Errorf("legacy cloud recovery identity differs")
			}
			var base tunnel.PeerListDoc
			if err := json.Unmarshal(plan.Base, &base); err != nil {
				return nil, err
			}
			for _, peer := range base.Peers {
				if !peer.Remote {
					local = append(local, peer)
				}
			}
		} else if site, found := plan.Sites[key.PublicKey().String()]; found && site.NodeUID == nodeUID {
			peer, err := receiveSiteLegacyPeer(plan, key.PublicKey().String())
			if err != nil {
				return nil, err
			}
			if site.Legacy == nil || site.Legacy.Transit == nil || (site.Legacy.Tunnel != nil && len(site.Legacy.Tunnel.Hosts) != 0) {
				local = append(local, peer)
			}
		}
		for _, peer := range local {
			if previous, found := seen[peer.PublicKey]; found {
				if !reflect.DeepEqual(previous, peer) {
					return nil, fmt.Errorf("selected legacy peer has conflicting plans")
				}
				continue
			}
			seen[peer.PublicKey] = peer
			peers = append(peers, peer)
		}
	}
	return peers, nil
}

// Restore only the selected legacy partition. Transport readback here checks
// configuration; the phase verifier separately requires current handshakes.
func restoreReceiveLegacyPeers(ctx context.Context, devices receiverDevices, cfg config, key wgtypes.Key, nodeUID string, intent tunnel.ReceiveJournalRoutes) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if devices == nil || key == (wgtypes.Key{}) || nodeUID == "" || cfg.fwmark <= 0 || cfg.keepaliveSecs < 0 || cfg.keepaliveSecs > 65535 {
		return fmt.Errorf("legacy recovery requires admitted local identity and keepalive")
	}
	peers, err := selectedReceiveLegacyPeers(key, nodeUID, intent)
	if err != nil || len(peers) == 0 {
		return err
	}
	live, err := devices.Device(cfg.iface)
	if err != nil {
		return err
	}
	if live.PublicKey != key.PublicKey() || live.FirewallMark != cfg.fwmark {
		return fmt.Errorf("legacy recovery root identity differs")
	}
	selected := map[wgtypes.Key]bool{}
	owners := map[netip.Prefix]wgtypes.Key{}
	var wanted []wgtypes.PeerConfig
	keepalive := time.Duration(cfg.keepaliveSecs) * time.Second
	for _, peer := range peers {
		public, err := wgtypes.ParseKey(peer.PublicKey)
		if err != nil || public == (wgtypes.Key{}) || public == key.PublicKey() || selected[public] || len(peer.WGAllowedIPs) == 0 {
			return fmt.Errorf("invalid selected legacy peer")
		}
		selected[public] = true
		var endpoint *net.UDPAddr
		if peer.Endpoint != "" {
			endpoint, err = net.ResolveUDPAddr("udp", peer.Endpoint)
			if err != nil || endpoint == nil || endpoint.IP == nil || endpoint.Port <= 0 {
				return fmt.Errorf("invalid selected legacy endpoint")
			}
		}
		var allowed []net.IPNet
		for _, raw := range peer.WGAllowedIPs {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil || prefix != prefix.Masked() || prefix.Bits() == 0 || prefix.Addr().Is4In6() || !prefix.Addr().IsGlobalUnicast() {
				return fmt.Errorf("invalid selected legacy sources")
			}
			for other, owner := range owners {
				if owner != public && prefix.Overlaps(other) {
					return fmt.Errorf("selected legacy source owners overlap")
				}
			}
			for _, old := range live.Peers {
				if old.PublicKey == public {
					continue
				}
				for _, raw := range old.AllowedIPs {
					other, err := netip.ParsePrefix(raw.String())
					if err != nil || prefix.Overlaps(other) {
						return fmt.Errorf("legacy recovery would take another peer's source permissions")
					}
				}
			}
			owners[prefix] = public
			_, parsed, _ := net.ParseCIDR(raw)
			allowed = append(allowed, *parsed)
		}
		for _, old := range live.Peers {
			if old.PublicKey != public {
				continue
			}
			if old.PresharedKey != (wgtypes.Key{}) {
				return fmt.Errorf("selected legacy peer has an unadmitted preshared key")
			}
			if endpoint != nil && old.Endpoint != nil && (!endpoint.IP.Equal(old.Endpoint.IP) || endpoint.Port != old.Endpoint.Port || endpoint.Zone != old.Endpoint.Zone) {
				return fmt.Errorf("selected legacy endpoint differs")
			}
			desired := prefixSet(allowed)
			for source := range prefixSet(old.AllowedIPs) {
				if !desired[source] {
					return fmt.Errorf("legacy recovery cannot withdraw live source permissions")
				}
			}
		}
		wanted = append(wanted, wgtypes.PeerConfig{PublicKey: public, AllowedIPs: allowed, ReplaceAllowedIPs: true, Endpoint: endpoint, PersistentKeepaliveInterval: &keepalive})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	partition := *live
	partition.Peers = nil
	for _, peer := range live.Peers {
		if selected[peer.PublicKey] {
			partition.Peers = append(partition.Peers, peer)
		}
	}
	if delta, changed := deviceDelta(wgtypes.Config{Peers: wanted}, &partition); changed {
		if err := devices.ConfigureDevice(cfg.iface, delta); err != nil {
			return err
		}
	}
	after, err := devices.Device(cfg.iface)
	if err != nil {
		return err
	}
	if after.PublicKey != live.PublicKey || after.FirewallMark != live.FirewallMark || after.ListenPort != live.ListenPort {
		return fmt.Errorf("legacy recovery changed root identity")
	}
	for _, want := range peers {
		found := false
		for _, peer := range after.Peers {
			if peer.PublicKey.String() == want.PublicKey {
				found = receiveSourcesMatch(want.WGAllowedIPs, peer.AllowedIPs)
			}
		}
		if !found {
			return fmt.Errorf("selected legacy peer readback differs")
		}
	}
	return ctx.Err()
}
