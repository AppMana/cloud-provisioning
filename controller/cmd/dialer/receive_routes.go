package main

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
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
		if !receiveSourcesMatch(selection.AllowedSources, peer.AllowedIPs) {
			return nil, fmt.Errorf("selected receiver source permissions differ from the committed plan")
		}
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

// Route coverage alone cannot detect a device that also accepts spoofed sources.
// Compare the complete canonical permission set, including retained drain paths.
func receiveSourcesMatch(expected []string, actual []net.IPNet) bool {
	if len(expected) == 0 || len(expected) != len(actual) {
		return false
	}
	canonical := func(raw []string) ([]string, bool) {
		result := make([]string, 0, len(raw))
		seen := map[netip.Prefix]bool{}
		for _, value := range raw {
			p, err := netip.ParsePrefix(value)
			if err != nil || p != p.Masked() || p.Bits() == 0 || p.Addr().Is4In6() || !p.Addr().IsGlobalUnicast() || seen[p] {
				return nil, false
			}
			seen[p] = true
			result = append(result, p.String())
		}
		slices.Sort(result)
		return result, true
	}
	want, valid := canonical(expected)
	if !valid {
		return false
	}
	raw := make([]string, len(actual))
	for i, p := range actual {
		raw[i] = p.String()
	}
	got, valid := canonical(raw)
	return valid && slices.Equal(want, got)
}

func validateReceiveRouteScope(cfg config, hosts, claims, blocks []net.IPNet, relay *tunnel.TransitSpec, relayDsts []net.IPNet) error {
	if cfg.receiveRouteScope == nil {
		return nil
	}
	if len(cfg.receiveRouteScope) == 0 || len(claims) != 0 || relay != nil || len(relayDsts) != 0 {
		return fmt.Errorf("scoped receiver routes require explicit direct destinations without unresolved claims")
	}
	for raw, owned := range cfg.receiveRouteScope {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || !owned || prefix != prefix.Masked() || prefix.Bits() == 0 || prefix.Addr().Is4In6() || !prefix.Addr().IsGlobalUnicast() {
			return fmt.Errorf("invalid receiver route scope")
		}
	}
	for _, list := range [][]net.IPNet{hosts, blocks} {
		for _, dst := range list {
			if !cfg.receiveRouteScope[dst.String()] {
				return fmt.Errorf("destination exceeds admitted receiver route scope")
			}
		}
	}
	return nil
}
