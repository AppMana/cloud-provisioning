package main

import (
	"fmt"
	"net"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// retireReceiveDevice removes one dedicated receiver after a committed abort
// or reverse-direction drain. The caller must hold the native lock and verify
// the selected replacement routes. Shared legacy devices use peer retirement
// instead. No address, route or unrelated peer is removed to make deletion pass.
func retireReceiveDevice(devices receiverDevices, selection receiveRouteSelection) error {
	local, err := wgtypes.ParseKey(selection.PublicKey)
	if err != nil || local == (wgtypes.Key{}) {
		return fmt.Errorf("receiver retirement requires local identity")
	}
	peer, err := wgtypes.ParseKey(selection.PeerPublicKey)
	if err != nil || peer == (wgtypes.Key{}) || peer == local || devices == nil {
		return fmt.Errorf("receiver retirement requires peer identity")
	}
	if selection.Interface == "" || (!strings.HasPrefix(selection.Alias, "cloud-provisioning:receive:v1:") && !strings.HasPrefix(selection.Alias, "cloud-provisioning:receive-client:v1:")) {
		return fmt.Errorf("receiver retirement requires dedicated device ownership")
	}
	var allowed []net.IPNet
	for _, raw := range selection.AllowedSources {
		_, prefix, err := net.ParseCIDR(raw)
		if err != nil {
			return err
		}
		allowed = append(allowed, *prefix)
	}
	if !receiveSourcesMatch(selection.AllowedSources, allowed) {
		return fmt.Errorf("invalid receiver retirement permissions")
	}
	link, err := netlink.LinkByName(selection.Interface)
	if isLinkNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	check := func() error {
		current, err := netlink.LinkByName(selection.Interface)
		if err != nil {
			return err
		}
		if current.Type() != "wireguard" || current.Attrs().Index != link.Attrs().Index || current.Attrs().Alias != selection.Alias {
			return fmt.Errorf("receiver retirement ownership changed")
		}
		addresses, err := netlink.AddrList(current, netlink.FAMILY_ALL)
		if err != nil {
			return err
		}
		if len(addresses) != 0 {
			return fmt.Errorf("receiver retirement would remove owned addresses")
		}
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		for _, route := range routes {
			if receiveRouteUsesLink(route, current.Attrs().Index) && !receiveDeviceKernelMulticast(route, current.Attrs().Index) {
				return fmt.Errorf("receiver retirement is blocked by a route in table %d", route.Table)
			}
		}
		live, err := devices.Device(selection.Interface)
		if err != nil {
			return err
		}
		if live.PublicKey != local || len(live.Peers) > 1 {
			return fmt.Errorf("receiver retirement device identity or membership changed")
		}
		if len(live.Peers) == 1 && (live.Peers[0].PublicKey != peer || !receiveSourcesMatch(selection.AllowedSources, live.Peers[0].AllowedIPs)) {
			return fmt.Errorf("receiver retirement peer or permissions changed")
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	if err := withdrawLegacyReceivePeer(devices, selection.Interface, selection.PublicKey, selection.PeerPublicKey, selection.AllowedSources); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	live, err := devices.Device(selection.Interface)
	if err != nil {
		return err
	}
	if len(live.Peers) != 0 {
		return fmt.Errorf("receiver still has peers after withdrawal")
	}
	if err := netlink.LinkDel(link); err != nil {
		return err
	}
	if _, err := netlink.LinkByName(selection.Interface); !isLinkNotFound(err) {
		return fmt.Errorf("receiver absence not established after retirement: %v", err)
	}
	return nil
}

// Linux installs this address-independent multicast route when a device comes
// up. It disappears with the device and is not a selected forwarding path.
func receiveDeviceKernelMulticast(route netlink.Route, index int) bool {
	return route.LinkIndex == index && route.Family == netlink.FAMILY_V6 &&
		route.Table == unix.RT_TABLE_LOCAL && route.Protocol == unix.RTPROT_KERNEL &&
		route.Type == unix.RTN_MULTICAST && route.Scope == netlink.SCOPE_UNIVERSE &&
		route.Dst != nil && route.Dst.String() == "ff00::/8" && route.Priority == 256 &&
		len(route.Gw) == 0 && len(route.Src) == 0 && len(route.MultiPath) == 0 &&
		route.Via == nil && route.Encap == nil && route.NewDst == nil && route.Tos == 0
}

func receiveRouteUsesLink(route netlink.Route, index int) bool {
	if route.LinkIndex == index {
		return true
	}
	for _, hop := range route.MultiPath {
		if hop.LinkIndex == index {
			return true
		}
	}
	return false
}
