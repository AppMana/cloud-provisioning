package tunneldevice

import (
	"fmt"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

var getBestTransportRoute = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetBestRoute2")

func transportPathMTU(destination netip.Addr, owned winipcfg.LUID) (uint32, error) {
	var address, source winipcfg.RawSockaddrInet
	if err := address.SetAddr(destination); err != nil {
		return 0, err
	}
	var route winipcfg.MibIPforwardRow2
	status, _, _ := getBestTransportRoute.Call(0, 0, 0, uintptr(unsafe.Pointer(&address)), 0, uintptr(unsafe.Pointer(&route)), uintptr(unsafe.Pointer(&source)))
	if status != 0 {
		return 0, fmt.Errorf("looking up transport route: %w", windows.Errno(status))
	}
	if route.InterfaceLUID == owned {
		return 0, fmt.Errorf("transport endpoint routes through the owned tunnel")
	}
	family := winipcfg.AddressFamily(windows.AF_INET)
	if destination.Is6() {
		family = windows.AF_INET6
	}
	row, err := route.InterfaceLUID.IPInterface(family)
	if err != nil {
		return 0, fmt.Errorf("reading transport interface MTU: %w", err)
	}
	return row.NLMTU, nil
}

func (n *Native) mtu(p Plan) (uint32, error) {
	var underlays []uint32
	needsDefaults := false
	for _, peer := range p.Peers {
		if !peer.Endpoint.IsValid() {
			needsDefaults = true
			continue
		}
		mtu, err := transportPathMTU(peer.Endpoint.Addr(), n.adapter.LUID())
		if err != nil {
			return 0, err
		}
		underlays = append(underlays, mtu)
	}
	// A listening peer has no configured destination yet. Bound its budget by
	// every operational default egress, including either transport family.
	if needsDefaults {
		routes, err := winipcfg.GetIPForwardTable2(windows.AF_UNSPEC)
		if err != nil {
			return 0, err
		}
		found := false
		for _, route := range routes {
			if route.DestinationPrefix.PrefixLength != 0 || route.InterfaceLUID == n.adapter.LUID() {
				continue
			}
			iface, err := route.InterfaceLUID.Interface()
			if err != nil || iface.OperStatus != winipcfg.IfOperStatusUp {
				continue
			}
			family := winipcfg.AddressFamily(windows.AF_INET)
			if route.DestinationPrefix.Prefix().Addr().Is6() {
				family = windows.AF_INET6
			}
			row, err := route.InterfaceLUID.IPInterface(family)
			if err != nil {
				return 0, err
			}
			underlays = append(underlays, row.NLMTU)
			found = true
		}
		if !found {
			return 0, fmt.Errorf("no operational default transport route for listening peer")
		}
	}
	return transportMTU(underlays)
}
