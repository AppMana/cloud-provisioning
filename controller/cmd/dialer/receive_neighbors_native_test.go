package main

import (
	"context"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestNativeReceiveNeighbors(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_RECEIVE_NEIGHBORS") != "1" {
		t.Skip("requires isolated lab network and mount namespaces")
	}
	for _, kind := range []string{"net", "mnt"} {
		self, err := os.Readlink("/proc/self/ns/" + kind)
		init, other := os.Readlink("/proc/1/ns/" + kind)
		if err != nil || other != nil || self == init {
			t.Fatal("refusing guest host namespace")
		}
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	parent, err := netns.Get()
	must(err)
	defer parent.Close()
	lo, err := netlink.LinkByName("lo")
	must(err)
	must(netlink.LinkSetUp(lo))
	root, err := wgtypes.GeneratePrivateKey()
	must(err)
	link := &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: "neighbors"}, LinkType: "wireguard"}
	must(netlink.LinkAdd(link))
	defer netlink.LinkDel(link)
	addAddrs(t, link, "10.77.0.1/32")
	must(netlink.LinkSetUp(link))
	devices, err := wgctrl.New()
	must(err)
	defer devices.Close()
	port, mark := 51821, 517
	must(devices.ConfigureDevice("neighbors", wgtypes.Config{PrivateKey: &root, ListenPort: &port, FirewallMark: &mark}))
	_, target, _ := net.ParseCIDR("10.77.0.1/32")
	makeSender := func(name, address string, port int) (wgtypes.Key, *net.UDPConn) {
		t.Helper()
		private, err := wgtypes.GeneratePrivateKey()
		must(err)
		child, err := netns.New()
		must(err)
		must(netns.Set(parent))
		t.Cleanup(func() { child.Close() })
		sender := &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: name}, LinkType: "wireguard"}
		must(netlink.LinkAdd(sender))
		must(devices.ConfigureDevice(name, wgtypes.Config{PrivateKey: &private, ListenPort: &port, Peers: []wgtypes.PeerConfig{{PublicKey: root.PublicKey(), Endpoint: &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 51821}, AllowedIPs: []net.IPNet{*target}}}}))
		must(netlink.LinkSetNsFd(sender, int(child)))
		var conn *net.UDPConn
		inNamespace(t, child, parent, func() {
			live, err := netlink.LinkByName(name)
			must(err)
			addAddrs(t, live, address+"/32")
			must(netlink.LinkSetUp(live))
			must(netlink.RouteAdd(&netlink.Route{LinkIndex: live.Attrs().Index, Dst: target, Scope: netlink.SCOPE_LINK}))
			must(setSysctl("ipv4/conf/all/rp_filter", "0"))
			must(setSysctl("ipv4/conf/"+name+"/rp_filter", "0"))
			conn, err = net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP(address)}, &net.UDPAddr{IP: target.IP, Port: 17777})
			must(err)
		})
		t.Cleanup(func() { conn.Close() })
		return private.PublicKey(), conn
	}
	protected, existing := makeSender("protected", "10.77.0.2", 51822)
	neighbor, joining := makeSender("joining", "10.77.0.3", 51823)
	_, protectedPrefix, _ := net.ParseCIDR("10.77.0.2/32")
	must(devices.ConfigureDevice("neighbors", wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: protected, Endpoint: &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 51822}, AllowedIPs: []net.IPNet{*protectedPrefix}}}}))
	protectedRoute := netlink.Route{LinkIndex: link.Attrs().Index, Dst: protectedPrefix, Src: net.ParseIP("10.77.0.1"), Table: 517, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT}
	must(netlink.RouteAdd(&protectedRoute))
	must(ensureRouteRule(517, 517))
	must(setSysctl("ipv4/conf/all/rp_filter", "0"))
	must(setSysctl("ipv4/conf/neighbors/rp_filter", "0"))
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: target.IP, Port: 17777})
	must(err)
	defer listener.Close()
	go func() {
		buffer := make([]byte, 128)
		for {
			n, remote, err := listener.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			if _, err := listener.WriteToUDP(buffer[:n], remote); err != nil {
				return
			}
		}
	}()
	exchange := func(conn *net.UDPConn) error {
		if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
			return err
		}
		if _, err := conn.Write([]byte("continuity")); err != nil {
			return err
		}
		_, err := conn.Read(make([]byte, 64))
		return err
	}
	probe := func(conn *net.UDPConn) {
		t.Helper()
		for i := 0; i < 10; i++ {
			must(exchange(conn))
		}
	}
	probe(existing)
	stop := make(chan struct{})
	done := make(chan struct{ attempts, failures int }, 1)
	go func() {
		result := struct{ attempts, failures int }{}
		for {
			select {
			case <-stop:
				done <- result
				return
			default:
			}
			result.attempts++
			if exchange(existing) != nil {
				result.failures++
			}
			time.Sleep(time.Millisecond)
		}
	}()
	defer func() {
		close(stop)
		result := <-done
		if result.attempts == 0 || result.failures != 0 {
			t.Errorf("protected stream: %d attempts, %d failures", result.attempts, result.failures)
		}
		t.Logf("protected stream: %d attempts, %d failures", result.attempts, result.failures)
	}()
	cfg := config{iface: "neighbors", routeTable: 517, fwmark: 517, keepaliveSecs: 5}
	peer := tunnel.PeerSpec{PublicKey: neighbor.String(), Remote: true, Endpoint: "127.0.0.1:51823", WGAllowedIPs: []string{"10.77.0.3/32", "10.77.1.0/24", "10.77.1.4/32"}, RouteHosts: []string{"10.77.0.3"}}
	must(installReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer}, []string{"10.77.0.1"}))
	probe(joining)
	// Reapplication must preserve both established sessions and foreign routes.
	must(installReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer}, []string{"10.77.0.1"}))
	probe(joining)
	for _, fault := range []string{"reserved-source", "changed-source", "foreign-route", "covering-route", "main-route", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			candidate := peer
			ctx := context.Background()
			switch fault {
			case "reserved-source":
				candidate.WGAllowedIPs = []string{"10.77.0.2/32"}
			case "changed-source":
				candidate.WGAllowedIPs = []string{"10.77.0.4/32"}
			case "foreign-route":
				_, dst, _ := net.ParseCIDR("10.77.0.3/32")
				r := netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst, Table: 517, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_STATIC, Priority: 42}
				must(netlink.RouteAdd(&r))
				defer netlink.RouteDel(&r)
			case "main-route":
				_, dst, _ := net.ParseCIDR("10.77.0.3/32")
				r := netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst, Table: unix.RT_TABLE_MAIN, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT}
				must(netlink.RouteAdd(&r))
				defer netlink.RouteDel(&r)
			case "covering-route":
				_, dst, _ := net.ParseCIDR("10.77.0.0/24")
				r := netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst, Table: 517, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_STATIC}
				must(netlink.RouteAdd(&r))
				defer netlink.RouteDel(&r)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := installReceiveNeighbors(ctx, devices, cfg, root, []tunnel.PeerSpec{candidate}, []string{"10.77.0.1"}); err == nil {
				t.Fatal("unsafe change accepted")
			}
		})
		probe(joining)
	}
	for i := 0; i < 50; i++ {
		must(installReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer}, []string{"10.77.0.1"}))
		time.Sleep(2 * time.Millisecond)
	}
}
