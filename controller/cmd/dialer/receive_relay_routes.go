package main

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Validate the whole relay selection before any route or rule mutation. Gateway
// resolution must be a single directly connected underlay path. Marked lookup
// uses the same bypass as tunnel outers; a recursive tunnel route cannot stand
// in for the relay's physical next hop.
func validateReceiveRelayRoutes(cfg config, hosts, blocks []net.IPNet) (map[string]int, error) {
	indices := map[string]int{}
	if len(cfg.receiveRelayRoutes) == 0 {
		return indices, nil
	}
	destinations := map[string]bool{}
	for _, list := range [][]net.IPNet{hosts, blocks} {
		for _, dst := range list {
			destinations[dst.String()] = true
		}
	}
	for raw, gateway := range cfg.receiveRelayRoutes {
		dst, err := netip.ParsePrefix(raw)
		addr, addrErr := netip.ParseAddr(gateway)
		_, receiver := cfg.receiveRoutes[raw]
		if err != nil || dst != dst.Masked() || dst.Bits() == 0 || !dst.Addr().IsGlobalUnicast() || dst.Addr().Is4In6() || addrErr != nil || !addr.IsGlobalUnicast() || addr.Is4In6() || addr.Is4() != dst.Addr().Is4() || dst.Contains(addr) || !cfg.receiveRouteScope[raw] || !destinations[raw] || receiver || cfg.fwmark <= 0 {
			return nil, fmt.Errorf("relay selection is not an exclusive admitted destination with a same-family next hop")
		}
	}
	for raw, gateway := range cfg.receiveRelayRoutes {
		paths, err := netlink.RouteGetWithOptions(net.ParseIP(gateway), &netlink.RouteGetOptions{Mark: uint32(cfg.fwmark)})
		if err != nil {
			return nil, err
		}
		if len(paths) != 1 {
			return nil, fmt.Errorf("relay gateway resolution is ambiguous")
		}
		path := paths[0]
		if path.Type != unix.RTN_UNICAST || path.LinkIndex <= 0 || len(path.Gw) != 0 || len(path.MultiPath) != 0 || path.Via != nil || path.Encap != nil || path.NewDst != nil {
			return nil, fmt.Errorf("relay gateway is not directly reachable")
		}
		link, err := netlink.LinkByIndex(path.LinkIndex)
		if err != nil {
			return nil, err
		}
		if link.Type() == "wireguard" || link.Attrs().Name == cfg.iface || link.Attrs().Flags&net.FlagUp == 0 || link.Attrs().Flags&net.FlagLoopback != 0 {
			return nil, fmt.Errorf("relay gateway does not use an active underlay link")
		}
		indices[raw] = path.LinkIndex
	}
	return indices, nil
}
