package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// forwardingRig is a router namespace (the test's own) between two
// leaf namespaces. The router's link toward the far leaf carries the
// tunnel's name, so the dialer's forward-hook rules match traffic
// crossing it exactly as they match traffic entering a WireGuard device.
type forwardingRig struct {
	near, far netns.NsHandle
	nearAddr  map[int]net.IP // by family
	farAddr   map[int]net.IP
}

func newForwardingRig(t *testing.T, tunnelName string) *forwardingRig {
	t.Helper()
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	router, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { router.Close() })
	rig := &forwardingRig{
		nearAddr: map[int]net.IP{netlink.FAMILY_V4: net.ParseIP("192.0.2.2"), netlink.FAMILY_V6: net.ParseIP("fd01::2")},
		farAddr:  map[int]net.IP{netlink.FAMILY_V4: net.ParseIP("198.51.100.2"), netlink.FAMILY_V6: net.ParseIP("fd02::2")},
	}
	type side struct {
		local, peer string
		routerV4    string
		routerV6    string
		leafV4      string
		leafV6      string
		handle      *netns.NsHandle
	}
	for _, s := range []side{
		{"n" + tunnelName, "leaf0", "192.0.2.1/24", "fd01::1/64", "192.0.2.2/24", "fd01::2/64", &rig.near},
		{tunnelName, "leaf0", "198.51.100.1/24", "fd02::1/64", "198.51.100.2/24", "fd02::2/64", &rig.far},
	} {
		leaf, err := netns.New()
		if err != nil {
			t.Fatalf("creating a leaf namespace: %v", err)
		}
		// netns.New switches this thread into the new namespace.
		if err := netns.Set(router); err != nil {
			t.Fatal(err)
		}
		*s.handle = leaf
		t.Cleanup(func() { leaf.Close() })
		veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: s.local}, PeerName: s.peer, PeerNamespace: netlink.NsFd(leaf)}
		if err := netlink.LinkAdd(veth); err != nil {
			t.Fatalf("adding %s: %v", s.local, err)
		}
		local, err := netlink.LinkByName(s.local)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = netlink.LinkDel(local) })
		addAddrs(t, local, s.routerV4, s.routerV6)
		if err := netlink.LinkSetUp(local); err != nil {
			t.Fatal(err)
		}
		inNamespace(t, leaf, router, func() {
			lo, _ := netlink.LinkByName("lo")
			_ = netlink.LinkSetUp(lo)
			peer, err := netlink.LinkByName(s.peer)
			if err != nil {
				t.Fatal(err)
			}
			addAddrs(t, peer, s.leafV4, s.leafV6)
			if err := netlink.LinkSetUp(peer); err != nil {
				t.Fatal(err)
			}
			// No timestamp option, so the segment size a connection
			// settles on is exactly the MSS its SYN carried.
			if err := os.WriteFile("/proc/sys/net/ipv4/tcp_timestamps", []byte("0"), 0o644); err != nil {
				t.Fatal(err)
			}
			gw4, _, _ := net.ParseCIDR(s.routerV4)
			gw6, _, _ := net.ParseCIDR(s.routerV6)
			for _, gw := range []net.IP{gw4, gw6} {
				if err := netlink.RouteAdd(&netlink.Route{LinkIndex: peer.Attrs().Index, Gw: gw}); err != nil {
					t.Fatalf("default route via %s: %v", gw, err)
				}
			}
		})
	}
	for _, knob := range []string{"/proc/sys/net/ipv4/ip_forward", "/proc/sys/net/ipv6/conf/all/forwarding"} {
		if err := os.WriteFile(knob, []byte("1"), 0o644); err != nil {
			t.Fatalf("enabling forwarding: %v", err)
		}
	}
	return rig
}

func addAddrs(t *testing.T, link netlink.Link, cidrs ...string) {
	t.Helper()
	for _, cidr := range cidrs {
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatal(err)
		}
		// No duplicate address detection: a tentative address cannot
		// source or accept anything for the first second or so.
		addr.Flags = unix.IFA_F_NODAD
		if err := netlink.AddrAdd(link, addr); err != nil {
			t.Fatalf("adding %s: %v", cidr, err)
		}
	}
}

// inNamespace runs f with this thread in ns, then returns to back. Sockets
// keep the namespace they were created in, so a listener or a dialed
// connection opened inside f stays there.
func inNamespace(t *testing.T, ns, back netns.NsHandle, f func()) {
	t.Helper()
	if err := netns.Set(ns); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := netns.Set(back); err != nil {
			t.Fatal(err)
		}
	}()
	f()
}

// segmentSize opens one TCP connection from the near leaf to the far one
// through the router and reports the segment size the far end settled
// on, which is bounded by the MSS option the near end's SYN carried
// when it arrived.
func (rig *forwardingRig) segmentSize(t *testing.T, family, port int) (int, error) {
	t.Helper()
	router, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	addr := net.JoinHostPort(rig.farAddr[family].String(), fmt.Sprint(port))
	var listener net.Listener
	inNamespace(t, rig.far, router, func() {
		listener, err = net.Listen("tcp", addr)
	})
	if err != nil {
		t.Fatalf("listening on %s: %v", addr, err)
	}
	defer listener.Close()
	accepted := make(chan int, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			accepted <- -1
			return
		}
		defer conn.Close()
		raw, _ := conn.(*net.TCPConn).SyscallConn()
		mss := -1
		_ = raw.Control(func(fd uintptr) {
			mss, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_MAXSEG)
		})
		accepted <- mss
	}()
	var conn net.Conn
	inNamespace(t, rig.near, router, func() {
		conn, err = net.DialTimeout("tcp", addr, 3*time.Second)
	})
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	select {
	case mss := <-accepted:
		return mss, nil
	case <-time.After(3 * time.Second):
		return 0, errors.New("the far end accepted nothing")
	}
}

// The segment size a session crossing the tunnel may use is the tunnel
// MTU less that family's IP and TCP headers: 40 bytes for IPv4 and 60
// for IPv6. Clamping only IPv4 leaves an IPv6 session negotiating a
// segment that cannot cross the tunnel, so it stalls on its first full
// segment until path MTU discovery, which the endpoint has to signal to
// a pod on another node, rescues it, if anything does.
func TestTheTunnelClampsEachFamilysSegmentSizeAgainstTheKernel(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside a namespace")
	}
	rig := newForwardingRig(t, "cldttest1")
	const mtu = 1420
	if err := ensureForwardingPath("cldttest1", mtu); err != nil {
		t.Fatalf("ensureForwardingPath: %v", err)
	}
	for family, want := range map[int]int{netlink.FAMILY_V4: mtu - 40, netlink.FAMILY_V6: mtu - 60} {
		got, err := rig.segmentSize(t, family, 8080)
		if err != nil {
			t.Fatalf("family %d: %v", family, err)
		}
		if got != want {
			t.Errorf("family %d settled on segment size %d across a %d-byte tunnel, want %d", family, got, mtu, want)
		}
	}

	// A sender whose own link is smaller already asks for less, and the
	// clamp must not raise it to the tunnel's size.
	router, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	inNamespace(t, rig.near, router, func() {
		leaf, err := netlink.LinkByName("leaf0")
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetMTU(leaf, 1300); err != nil {
			t.Fatal(err)
		}
	})
	for family, want := range map[int]int{netlink.FAMILY_V4: 1300 - 40, netlink.FAMILY_V6: 1300 - 60} {
		got, err := rig.segmentSize(t, family, 8081)
		if err != nil {
			t.Fatalf("family %d: %v", family, err)
		}
		if got != want {
			t.Errorf("family %d: a 1300-byte sender settled on segment size %d, want its own %d", family, got, want)
		}
	}
}

// BGP is refused at the tunnel in both families. Calico peers every
// node over IPv6 as well as IPv4 on a dual-stack site, so an IPv4-only
// refusal lets an IPv6 session establish across the tunnel and carry
// the routes the mesh deliberately keeps out of it.
func TestTheTunnelRefusesBGPInEachFamilyAgainstTheKernel(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside a namespace")
	}
	rig := newForwardingRig(t, "cldttest2")
	if err := ensureForwardingPath("cldttest2", 1420); err != nil {
		t.Fatalf("ensureForwardingPath: %v", err)
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		if _, err := rig.segmentSize(t, family, 179); err == nil {
			t.Errorf("family %d: a session to port 179 crossed the tunnel", family)
		}
		// The same path still carries everything else.
		if _, err := rig.segmentSize(t, family, 1790); err != nil {
			t.Errorf("family %d: an ordinary session did not cross: %v", family, err)
		}
	}
}

// A site node with no tunnel installs every remote prefix toward the
// relay, each through the relay's address of the prefix's own family.
// The kernel refuses an IPv6 route through an IPv4 gateway, so a single
// next hop installs nothing for IPv6 and aborts the pass at the first
// IPv6 prefix.
func TestSiteTransitRoutesUseEachFamilysGatewayAgainstTheKernel(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside a namespace")
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "cldtlan0"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatal(err)
	}
	defer netlink.LinkDel(link)
	addAddrs(t, link, "10.101.0.3/24", "fd8f:cf26:522a::a:12/64")
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
	transit := &tunnel.TransitSpec{
		Via: "10.101.0.2", ViaOtherFamily: "fd8f:cf26:522a::a:11",
		Hosts:  []string{"10.100.0.128", "fd00:10:100::a64:80"},
		Blocks: []string{"10.101.200.0/26", "fd8f:cf26:522a:128:abcd::/122"},
	}
	cfg := config{iface: "cldtwg0", routeTable: 517}
	if err := installSiteTransit(cfg, transit, nil); err != nil {
		t.Fatalf("installSiteTransit: %v", err)
	}
	for family, want := range map[int]map[string]string{
		netlink.FAMILY_V4: {"10.100.0.128/32": "10.101.0.2", "10.101.200.0/26": "10.101.0.2"},
		netlink.FAMILY_V6: {"fd00:10:100::a64:80/128": "fd8f:cf26:522a::a:11", "fd8f:cf26:522a:128:abcd::/122": "fd8f:cf26:522a::a:11"},
	} {
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: 517}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, r := range routes {
			if r.Dst != nil && r.Gw != nil {
				got[r.Dst.String()] = r.Gw.String()
			}
		}
		for dst, gw := range want {
			if got[dst] != gw {
				t.Errorf("table 517 routes %s via %q, want %s", dst, got[dst], gw)
			}
		}
	}
}

// A dual-stack mesh gives the tunnel an address in each family, and the
// device carries exactly those: an IPv6 address left over from an
// earlier allocation is a source no peer permits, exactly as a stale
// IPv4 one is.
func TestTheTunnelCarriesOneAddressPerFamilyAgainstTheKernel(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside a namespace")
	}
	cfg := config{iface: "cldtwgtest0", mtu: 1420}
	t.Cleanup(func() { removeDevice(cfg.iface) })
	addresses := func() map[string]bool {
		link, err := netlink.LinkByName(cfg.iface)
		if err != nil {
			t.Fatal(err)
		}
		list, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, a := range list {
			if !a.IP.IsLinkLocalUnicast() {
				got[a.IPNet.String()] = true
			}
		}
		return got
	}
	if err := ensureLink(cfg, []string{"10.100.0.1/24", "fd00:10:100::a64:1/96"}); err != nil {
		t.Fatalf("ensureLink: %v", err)
	}
	got := addresses()
	for _, want := range []string{"10.100.0.1/24", "fd00:10:100::a64:1/96"} {
		if !got[want] {
			t.Errorf("the tunnel lacks %s: %v", want, got)
		}
	}
	// A mesh that stops being dual-stack takes the IPv6 address away.
	if err := ensureLink(cfg, []string{"10.100.0.1/24"}); err != nil {
		t.Fatalf("ensureLink: %v", err)
	}
	if got := addresses(); len(got) != 1 || !got["10.100.0.1/24"] {
		t.Errorf("the tunnel carries %v, want only 10.100.0.1/24", got)
	}
}

// A site node known by an identity address on a dummy device sends to a
// remote through the relay from that identity, not from its LAN address.
// The kernel picks a route's source from its outgoing device, the LAN, and
// the remote accepts from the relay only the node's own addresses: the
// relay's transit entry carries those, not the LAN's. Measured on the
// dual-stack lab with the tunnel on one worker: the control plane's API
// server could not reach a remote kubelet (logs, exec) while every pod
// check passed, since pods source from their pod blocks.
func TestSiteTransitRoutesSourceFromTheNodesOwnAddressAgainstTheKernel(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside a namespace")
	}
	lan := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "cldtlan1"}}
	vip := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "cldtvip1"}}
	for _, l := range []netlink.Link{lan, vip} {
		if err := netlink.LinkAdd(l); err != nil {
			t.Fatal(err)
		}
		defer netlink.LinkDel(l)
	}
	addAddrs(t, lan, "10.10.0.10/24", "fd8f:cf26:522a::a:10/64")
	addAddrs(t, vip, "10.101.0.1/32", "fd8f:cf26:522a::1/128")
	for _, l := range []netlink.Link{lan, vip} {
		if err := netlink.LinkSetUp(l); err != nil {
			t.Fatal(err)
		}
	}
	_, identities, _ := net.ParseCIDR("10.101.0.0/24")
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: lan.Attrs().Index, Dst: identities, Scope: netlink.SCOPE_LINK}); err != nil {
		t.Fatal(err)
	}
	transit := &tunnel.TransitSpec{
		Via: "10.101.0.2", ViaOtherFamily: "fd8f:cf26:522a::a:11",
		Hosts:  []string{"203.0.113.10", "2001:db8:a::10"},
		Blocks: []string{"10.101.156.0/26"},
	}
	cfg := config{iface: "cldtwg1", routeTable: 518}
	// The node's own addresses, and one this host does not carry, which
	// no route can source from.
	own := []string{"10.101.0.1", "fd8f:cf26:522a::1", "10.101.0.9"}
	if err := installSiteTransit(cfg, transit, own); err != nil {
		t.Fatalf("installSiteTransit: %v", err)
	}
	for dst, want := range map[string]string{
		"203.0.113.10": "10.101.0.1", "10.101.156.1": "10.101.0.1", "2001:db8:a::10": "fd8f:cf26:522a::1",
	} {
		routes, err := netlink.RouteGet(net.ParseIP(dst))
		if err != nil {
			t.Fatalf("route to %s: %v", dst, err)
		}
		if len(routes) == 0 || routes[0].Src.String() != want {
			t.Errorf("traffic to %s sources from %v, want %s", dst, routes, want)
		}
	}
}
