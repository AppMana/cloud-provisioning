package main

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
)

// Exact kernel identities accompany every selected route. Selecting an
// interface must not configure its peer or move receive permissions.
type receiveRouteSelection = tunnel.ReceiveRouteSelection

// Validate the whole selection before installRoutes changes rules or routes.
// Destinations must already belong to the caller's validated tunnel route set.
func validateReceiveRouteSelections(cfg config, hosts, blocks []net.IPNet) (map[string]int, error) {
	result := map[string]int{}
	if len(cfg.receiveRoutes) == 0 {
		return result, nil
	}
	destinations := map[string]bool{}
	for _, list := range [][]net.IPNet{hosts, blocks} {
		for _, dst := range list {
			destinations[dst.String()] = true
		}
	}
	devices, err := wgctrl.New()
	if err != nil {
		return nil, err
	}
	defer devices.Close()
	for raw, selection := range cfg.receiveRoutes {
		dst, err := netip.ParsePrefix(raw)
		if err != nil || dst != dst.Masked() || dst.Bits() == 0 || !destinations[raw] {
			return nil, fmt.Errorf("receiver selection is not an existing explicit tunnel destination")
		}
		if selection.Interface == cfg.iface || selection.PublicKey == "" || selection.PeerPublicKey == "" || (!strings.HasPrefix(selection.Alias, "cloud-provisioning:receive:v1:") && !strings.HasPrefix(selection.Alias, "cloud-provisioning:receive-client:v1:")) {
			return nil, fmt.Errorf("receiver selection lacks independent owned identities")
		}
		link, err := netlink.LinkByName(selection.Interface)
		if err != nil {
			return nil, err
		}
		if link.Type() != "wireguard" || link.Attrs().Alias != selection.Alias || link.Attrs().Flags&net.FlagUp == 0 {
			return nil, fmt.Errorf("selected receiver device ownership or state changed")
		}
		device, err := devices.Device(selection.Interface)
		if err != nil {
			return nil, err
		}
		if device.PublicKey.String() != selection.PublicKey || device.FirewallMark != cfg.fwmark || cfg.fwmark <= 0 || len(device.Peers) != 1 || device.Peers[0].PublicKey.String() != selection.PeerPublicKey {
			return nil, fmt.Errorf("selected receiver cryptographic identity changed")
		}
		peer := device.Peers[0]
		now := time.Now()
		if peer.Endpoint == nil || peer.LastHandshakeTime.IsZero() || peer.LastHandshakeTime.After(now) || now.Sub(peer.LastHandshakeTime) > receiveHandshakeMaxAge {
			return nil, fmt.Errorf("selected receiver has no current authenticated transport")
		}
		covered := false
		for _, allowed := range peer.AllowedIPs {
			prefix, err := netip.ParsePrefix(allowed.String())
			if err == nil && prefix.Addr().BitLen() == dst.Addr().BitLen() && prefix.Bits() <= dst.Bits() && prefix.Contains(dst.Addr()) {
				covered = true
			}
		}
		if !covered {
			return nil, fmt.Errorf("selected receiver cannot carry the route destination")
		}
		result[raw] = link.Attrs().Index
	}
	return result, nil
}
