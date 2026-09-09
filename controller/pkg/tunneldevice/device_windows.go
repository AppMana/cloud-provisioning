package tunneldevice

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/driver"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

type Native struct {
	adapter *driver.Adapter
	routes  map[netip.Prefix]bool
	address netip.Prefix
}

var _ Device = (*Native)(nil)

// New creates an exclusively owned adapter. It never adopts an existing device.
// The signed WireGuardNT DLL must be installed alongside the executable.
func New(name string) (*Native, error) {
	if !regexp.MustCompile(`^cldt[0-9a-f]{8}$`).MatchString(name) {
		return nil, fmt.Errorf("invalid mesh interface name")
	}
	adapter, err := driver.CreateAdapter(name, "WireGuard", nil)
	if err != nil {
		return nil, err
	}
	return &Native{adapter: adapter, routes: map[netip.Prefix]bool{}}, nil
}
func (n *Native) Close() error { return n.adapter.Close() }

// KernelRoutes reads the OS table, including Windows-created link-local and
// multicast routes. It does not report the backend's desired-state cache.
func (n *Native) KernelRoutes() ([]netip.Prefix, error) {
	rows, err := winipcfg.GetIPForwardTable2(windows.AF_UNSPEC)
	if err != nil {
		return nil, err
	}
	var routes []netip.Prefix
	for _, row := range rows {
		if row.InterfaceLUID == n.adapter.LUID() {
			routes = append(routes, row.DestinationPrefix.Prefix())
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].String() < routes[j].String() })
	return routes, nil
}
func nextHop(p netip.Prefix) netip.Addr {
	if p.Addr().Is4() {
		return netip.IPv4Unspecified()
	}
	return netip.IPv6Unspecified()
}
func (n *Native) Apply(doc tunnel.PeersFileDoc, port uint16) error {
	p, err := Compile(doc)
	if err != nil {
		return err
	}
	if n.address.IsValid() && n.address != p.Address {
		return fmt.Errorf("adapter identity address is immutable")
	}
	// This backend owns its identity address. A separate generation backend
	// must model a shared address owner explicitly, rather than silently
	// assigning that address to a second adapter and invalidating the first.
	addresses, err := winipcfg.GetUnicastIPAddressTable(windows.AF_UNSPEC)
	if err != nil {
		return err
	}
	for _, address := range addresses {
		if address.Address.Addr() == p.Address.Addr() && address.InterfaceLUID != n.adapter.LUID() {
			return fmt.Errorf("tunnel identity address %s is already assigned to another interface", p.Address.Addr())
		}
	}
	// Read the native peer set so retries and partial failures cannot retain a
	// retired peer. Updating a surviving peer must not destroy its session.
	observedPeers, err := n.adapter.Configuration()
	if err != nil {
		return err
	}
	current := make([][32]byte, 0, observedPeers.PeerCount)
	peer := observedPeers.FirstPeer()
	for i := uint32(0); i < observedPeers.PeerCount; i++ {
		current = append(current, peer.PublicKey)
		peer = peer.NextPeer()
	}
	cfg, size, err := buildPeerUpdate(p, port, current)
	if err != nil {
		return err
	}
	if err = n.adapter.SetConfiguration(cfg, size); err != nil {
		return err
	}
	if err = n.adapter.SetAdapterState(driver.AdapterStateUp); err != nil {
		return err
	}
	luid := n.adapter.LUID()
	for _, family := range []winipcfg.AddressFamily{windows.AF_INET, windows.AF_INET6} {
		row, e := luid.IPInterface(family)
		if e != nil {
			return e
		}
		row.NLMTU = 1420
		row.UseAutomaticMetric = false
		row.Metric = 5
		row.WeakHostSend = true
		row.WeakHostReceive = true
		// As in upstream WireGuard's configureInterface: tunnel identities
		// come from the controller's allocation, not link-local discovery.
		// Configure this before adding addresses; optimistic DAD is still
		// DAD and can invalidate an address during generation overlap.
		row.DadTransmits = 0
		if err = row.Set(); err != nil {
			return err
		}
	}
	// The cached address enforces immutable identity; it is not proof that
	// Windows still has the address after a PnP event or external removal.
	if _, err = luid.IPAddress(p.Address.Addr()); err != nil {
		if !errors.Is(err, windows.ERROR_NOT_FOUND) {
			return err
		}
		if err = luid.AddIPAddress(p.Address); err != nil && !errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) {
			return err
		}
	}
	n.address = p.Address
	observed, err := n.KernelRoutes()
	if err != nil {
		return err
	}
	present := map[netip.Prefix]bool{}
	for _, route := range observed {
		present[route] = true
	}
	desired := map[netip.Prefix]bool{}
	for _, route := range p.Routes {
		desired[route] = true
		if !present[route] {
			if err = luid.AddRoute(route, nextHop(route), 5); err != nil && !errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) {
				return err
			}
		}
		n.routes[route] = true
	}
	for route := range n.routes {
		if !desired[route] {
			if err = luid.DeleteRoute(route, nextHop(route)); err != nil && !errors.Is(err, windows.ERROR_NOT_FOUND) {
				return err
			}
			delete(n.routes, route)
		}
	}
	return n.Verify(doc)
}

// Verify observes kernel routes before the host renews an applied receipt.
// Windows-created multicast/link-local routes are outside our ownership.
func (n *Native) Verify(doc tunnel.PeersFileDoc) error {
	p, err := Compile(doc)
	if err != nil {
		return err
	}
	address, err := n.adapter.LUID().IPAddress(p.Address.Addr())
	if err != nil {
		return fmt.Errorf("reading tunnel identity address: %w", err)
	}
	if address.DadState != winipcfg.DadStatePreferred {
		return fmt.Errorf("tunnel identity address is not preferred: DAD state %d", address.DadState)
	}
	observed, err := n.KernelRoutes()
	if err != nil {
		return err
	}
	return VerifyRoutes(p.Routes, n.routes, observed)
}

// buildPeerUpdate preserves live sessions for peers that remain desired.
func buildPeerUpdate(p Plan, port uint16, current [][32]byte) (*driver.Interface, uint32, error) {
	wanted := map[[32]byte]bool{}
	for _, peer := range p.Peers {
		wanted[peer.Key] = true
	}
	var retired [][32]byte
	for _, key := range current {
		if !wanted[key] {
			retired = append(retired, key)
		}
	}
	builder := driver.ConfigBuilder{}
	builder.AppendInterface(&driver.Interface{Flags: driver.InterfaceHasPrivateKey | driver.InterfaceHasListenPort, PrivateKey: p.Key, ListenPort: port, PeerCount: uint32(len(p.Peers) + len(retired))})
	for _, key := range retired {
		builder.AppendPeer(&driver.Peer{Flags: driver.PeerHasPublicKey | driver.PeerRemove, PublicKey: key})
	}
	for _, peer := range p.Peers {
		d := driver.Peer{Flags: driver.PeerHasPublicKey | driver.PeerHasPersistentKeepalive | driver.PeerReplaceAllowedIPs, PublicKey: peer.Key, PersistentKeepalive: 25, AllowedIPsCount: uint32(len(peer.Allowed))}
		if peer.Endpoint.IsValid() {
			d.Flags |= driver.PeerHasEndpoint
			if err := d.Endpoint.SetAddrPort(peer.Endpoint); err != nil {
				return nil, 0, err
			}
		}
		builder.AppendPeer(&d)
		for _, prefix := range peer.Allowed {
			ip := driver.AllowedIP{Cidr: uint8(prefix.Bits()), AddressFamily: windows.AF_INET6}
			if prefix.Addr().Is4() {
				ip.AddressFamily = windows.AF_INET
				a := prefix.Addr().As4()
				copy(ip.Address[:], a[:])
			} else {
				ip.Address = prefix.Addr().As16()
			}
			builder.AppendAllowedIP(&ip)
		}
	}
	cfg, size := builder.Interface()
	return cfg, size, nil
}
