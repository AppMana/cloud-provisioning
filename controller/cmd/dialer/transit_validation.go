package main

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var (
	reservedNonGlobalPrefixes = []struct {
		prefix netip.Prefix
		label  string
	}{
		{netip.MustParsePrefix("127.0.0.0/8"), "IPv4 loopback (127.0.0.0/8)"},
		{netip.MustParsePrefix("::1/128"), "IPv6 loopback (::1/128)"},
		{netip.MustParsePrefix("169.254.0.0/16"), "IPv4 link-local / IMDS (169.254.0.0/16)"},
		{netip.MustParsePrefix("fe80::/10"), "IPv6 link-local (fe80::/10)"},
		{netip.MustParsePrefix("224.0.0.0/4"), "IPv4 multicast (224.0.0.0/4)"},
		{netip.MustParsePrefix("ff00::/8"), "IPv6 multicast (ff00::/8)"},
		{netip.MustParsePrefix("0.0.0.0/32"), "IPv4 unspecified (0.0.0.0/32)"},
		{netip.MustParsePrefix("::/128"), "IPv6 unspecified (::/128)"},
		{netip.MustParsePrefix("255.255.255.255/32"), "IPv4 broadcast (255.255.255.255/32)"},
	}

	tailscaleManagementPrefixes = []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
	}
)

func ipNetToPrefix(ipNet net.IPNet) (netip.Prefix, bool) {
	ones, bits := ipNet.Mask.Size()
	if bits == 0 && len(ipNet.IP) == 0 {
		return netip.Prefix{}, false
	}
	if ip4 := ipNet.IP.To4(); ip4 != nil && bits == 32 {
		addr, ok := netip.AddrFromSlice(ip4)
		if !ok {
			return netip.Prefix{}, false
		}
		return netip.PrefixFrom(addr, ones).Masked(), true
	}
	addr, ok := netip.AddrFromSlice(ipNet.IP.To16())
	if !ok {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(addr, ones).Masked(), true
}

func checkNonReservedPrefix(ipNet net.IPNet) error {
	p, ok := ipNetToPrefix(ipNet)
	if !ok {
		return fmt.Errorf("invalid IP prefix %s", ipNet.String())
	}
	if p.Bits() == 0 {
		return fmt.Errorf("refusing %s: a default route takes every packet", ipNet.String())
	}
	for _, r := range reservedNonGlobalPrefixes {
		if p.Overlaps(r.prefix) {
			return fmt.Errorf("refusing %s: overlaps reserved %s", ipNet.String(), r.label)
		}
	}
	return nil
}

// defaultGatewayIPs returns all next-hop gateway IPs on default routes in the
// kernel's main routing table so tunnel policy table 517 can never hijack the
// host's underlay gateway.
func defaultGatewayIPs() []net.IP {
	var out []net.IP
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
		if err != nil {
			continue
		}
		for _, r := range routes {
			if r.Gw == nil {
				continue
			}
			if r.Dst != nil {
				ones, _ := r.Dst.Mask.Size()
				if ones != 0 {
					continue
				}
			}
			out = append(out, r.Gw)
		}
	}
	return out
}

// localNonTunnelIPs returns all non-loopback IP addresses assigned to
// non-WireGuard interfaces on this host (plus ownTunnelIface when non-empty).
func localNonTunnelIPs(ownTunnelIface string) []net.IP {
	links, err := netlink.LinkList()
	if err != nil {
		return localIPs()
	}
	var out []net.IP
	for _, link := range links {
		if link.Attrs() == nil {
			continue
		}
		if link.Type() == "wireguard" && (ownTunnelIface == "" || link.Attrs().Name != ownTunnelIface) {
			continue
		}
		addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if a.IP != nil && !a.IP.IsLoopback() {
				out = append(out, a.IP)
			}
		}
	}
	return out
}

// tailscaleProtectedPrefixes returns Tailscale's CGNAT and ULA prefixes when a
// Tailscale management interface (tailscale0) is present on the host. Because
// the tunnel policy rule sits at priority 517 (ahead of Tailscale's priority
// 5200/5270 table 52 lookup), any route in table 517 overlapping Tailscale's
// prefix space would hijack remote management SSH/API sessions.
func tailscaleProtectedPrefixes() []netip.Prefix {
	if _, err := net.InterfaceByName("tailscale0"); err == nil {
		return tailscaleManagementPrefixes
	}
	return nil
}

func validateProtectedDestination(ipNet net.IPNet, localAddrs, gatewayIPs []net.IP, mgmtPrefixes []netip.Prefix) error {
	if err := checkNonReservedPrefix(ipNet); err != nil {
		return err
	}
	for _, addr := range localAddrs {
		if addr != nil && !addr.IsLoopback() && ipNet.Contains(addr) {
			return fmt.Errorf("refusing %s: it covers this node's own address %s", ipNet.String(), addr)
		}
	}
	for _, gw := range gatewayIPs {
		if gw != nil && ipNet.Contains(gw) {
			return fmt.Errorf("refusing %s: it covers this node's default gateway %s", ipNet.String(), gw)
		}
	}
	if p, ok := ipNetToPrefix(ipNet); ok {
		for _, mgmt := range mgmtPrefixes {
			if p.Overlaps(mgmt) {
				return fmt.Errorf("refusing %s: it overlaps management VPN range %s", ipNet.String(), mgmt)
			}
		}
	}
	return nil
}

// Validate the entire input before changing policy rules or any route.
func validateSiteTransit(transit *tunnel.TransitSpec) error {
	if transit == nil {
		return nil
	}
	if net.ParseIP(transit.Via) == nil || (transit.ViaOtherFamily != "" && net.ParseIP(transit.ViaOtherFamily) == nil) {
		return fmt.Errorf("invalid transit gateway")
	}
	self := false
	local := localIPs()
	for _, ip := range local {
		for _, via := range []string{transit.Via, transit.ViaOtherFamily} {
			if via != "" && ip.Equal(net.ParseIP(via)) {
				self = true
			}
		}
	}
	nonTunnelLocal := localNonTunnelIPs("")
	gateways := defaultGatewayIPs()
	mgmtPrefixes := tailscaleProtectedPrefixes()
	for _, h := range transit.Hosts {
		ipNet, err := parseHostRoute(h)
		if err != nil {
			return err
		}
		if !self {
			if err := validateProtectedDestination(ipNet, nonTunnelLocal, gateways, mgmtPrefixes); err != nil {
				return err
			}
		}
	}
	for _, b := range transit.Blocks {
		if _, err := blockRoute(b, 0); err != nil {
			return err
		}
		_, ipNet, err := net.ParseCIDR(strings.TrimSpace(b))
		if err != nil {
			return err
		}
		if !self {
			if err := validateProtectedDestination(*ipNet, nonTunnelLocal, gateways, mgmtPrefixes); err != nil {
				return err
			}
		}
	}
	return nil
}
