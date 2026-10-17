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
	addAddrs(t, link, "10.77.0.1/24")
	must(netlink.LinkSetUp(link))
	devices, err := wgctrl.New()
	must(err)
	defer devices.Close()
	port, mark := 51821, 517
	must(devices.ConfigureDevice("neighbors", wgtypes.Config{PrivateKey: &root, ListenPort: &port, FirewallMark: &mark}))
	_, target, _ := net.ParseCIDR("10.77.0.1/32")
	makeSender := func(name, address string, port int) (wgtypes.Key, *net.UDPConn, netns.NsHandle) {
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
			mask, network := "/32", "udp4"
			if net.ParseIP(address).To4() == nil {
				mask, network = "/128", "udp6"
			}
			addAddrs(t, live, address+mask)
			must(netlink.LinkSetUp(live))
			must(netlink.RouteAdd(&netlink.Route{LinkIndex: live.Attrs().Index, Dst: target, Scope: netlink.SCOPE_LINK}))
			must(setSysctl("ipv4/conf/all/rp_filter", "0"))
			must(setSysctl("ipv4/conf/"+name+"/rp_filter", "0"))
			conn, err = net.DialUDP(network, &net.UDPAddr{IP: net.ParseIP(address)}, &net.UDPAddr{IP: target.IP, Port: 17777})
			must(err)
		})
		t.Cleanup(func() { conn.Close() })
		return private.PublicKey(), conn, child
	}
	protected, existing, _ := makeSender("protected", "10.77.0.2", 51822)
	neighbor, joining, joiningNS := makeSender("joining", "10.77.0.3", 51823)
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
	cfg := config{iface: "neighbors", routeTable: 517, fwmark: 517, keepaliveSecs: 5}
	verifyNativePolicyRecovery(t, devices, cfg, root, protected, protectedRoute)
	probe(existing)
	// Recovery must make its routes ready before the broader policy repair can
	// validate the whole mesh. It must not reinstall shared lookup rules itself.
	peer := tunnel.PeerSpec{PublicKey: neighbor.String(), Remote: true, Endpoint: "127.0.0.1:51823", WGAllowedIPs: []string{"10.77.0.3/32", "10.77.1.0/24", "10.77.1.4/32"}, RouteHosts: []string{"10.77.0.3"}}
	removeRouteRule(cfg.routeTable, cfg.fwmark)
	if err := installReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer}, []string{"10.77.0.1"}); err == nil {
		t.Fatal("ordinary installation accepted missing shared policy")
	}
	must(restoreReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer}, []string{"10.77.0.1"}))
	if err := verifyReceivePolicyRules(cfg); err == nil {
		t.Fatal("partition recovery installed shared lookup rules")
	}
	must(ensureRouteRule(cfg.routeTable, cfg.fwmark))
	probe(existing)
	probe(joining)
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

	must(installReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer}, []string{"10.77.0.1"}))
	probe(joining)
	// Reapplication must preserve both established sessions and foreign routes.
	// A node's pod CIDR can arrive after its initial host-only publication.
	peer.WGAllowedIPs = append(peer.WGAllowedIPs, "10.77.2.0/24")
	must(installReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer}, []string{"10.77.0.1"}))
	probe(joining)
	inNamespace(t, joiningNS, parent, func() {
		link, err := netlink.LinkByName("joining")
		must(err)
		addAddrs(t, link, "10.77.2.3/32")
		conn, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP("10.77.2.3")}, &net.UDPAddr{IP: target.IP, Port: 17777})
		must(err)
		defer conn.Close()
		probe(conn)
	})
	for _, fault := range []string{"reserved-source", "changed-source", "foreign-route", "foreign-rule", "covering-route", "main-route", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
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
			case "foreign-rule":
				rule := netlink.NewRule()
				rule.Family, rule.Table, rule.Priority = netlink.FAMILY_V4, cfg.routeTable, cfg.routeTable+10
				must(netlink.RuleAdd(rule))
				defer netlink.RuleDel(rule)
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
			if err := restoreReceiveNeighbors(ctx, devices, cfg, root, []tunnel.PeerSpec{candidate}, []string{"10.77.0.1"}); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
		})
		probe(joining)
	}
	for i := 0; i < 50; i++ {
		must(installReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer}, []string{"10.77.0.1"}))
		time.Sleep(2 * time.Millisecond)
	}
	// An obsolete neighbor may release its address only after all dependent
	// routes are proven owned. The unaffected peer's stream stays running.
	_, retiredDst, _ := net.ParseCIDR("10.77.0.3/32")
	foreign := netlink.Route{LinkIndex: link.Attrs().Index, Dst: retiredDst, Table: 999, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_STATIC}
	must(netlink.RouteAdd(&foreign))
	if err := retireReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer}, []string{"10.77.0.1"}, nil); err == nil {
		t.Fatal("retirement removed a peer with a foreign route dependency")
	}
	probe(joining)
	must(netlink.RouteDel(&foreign))
	if err := retireReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer}, []string{"10.77.0.1"}, nil); err == nil {
		t.Fatal("retirement adopted an unadmitted connected subnet")
	}
	probe(joining)
	allocated := []string{"10.77.0.1/24"}
	must(retireReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer}, []string{"10.77.0.1"}, allocated))
	// Repeating a partially or fully completed deletion must be harmless.
	must(retireReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer}, []string{"10.77.0.1"}, allocated))
	connected, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN, LinkIndex: link.Attrs().Index}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
	must(err)
	shared := false
	for _, route := range connected {
		shared = shared || (route.Dst != nil && route.Dst.String() == "10.77.0.0/24" && route.Protocol == unix.RTPROT_KERNEL)
	}
	if !shared {
		t.Fatal("neighbor retirement removed the shared connected route")
	}
	device, err := devices.Device(cfg.iface)
	must(err)
	for _, current := range device.Peers {
		if current.PublicKey == neighbor {
			t.Fatal("retired source owner remains installed")
		}
	}
	// Interrupted growth must permit deletion of either recorded exact state,
	// while an unrecorded subset or superset remains foreign. This isolated
	// kernel test does not establish VM reboot or cloud lifecycle continuity.
	grown := peer
	grown.WGAllowedIPs = append(append([]string{}, peer.WGAllowedIPs...), "10.77.0.30/32")
	variants := []tunnel.PeerSpec{peer, grown}
	for _, installed := range []tunnel.PeerSpec{peer, grown} {
		must(installReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{installed}, []string{"10.77.0.1"}))
		must(retireReceiveNeighbors(context.Background(), devices, cfg, root, variants, []string{"10.77.0.1"}, allocated))
		must(retireReceiveNeighbors(context.Background(), devices, cfg, root, variants, []string{"10.77.0.1"}, allocated))
	}
	foreignGrowth := peer
	foreignGrowth.WGAllowedIPs = append(append([]string{}, grown.WGAllowedIPs...), "10.77.0.31/32")
	must(installReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{foreignGrowth}, []string{"10.77.0.1"}))
	if err := retireReceiveNeighbors(context.Background(), devices, cfg, root, variants, []string{"10.77.0.1"}, allocated); err == nil {
		t.Fatal("unrecorded growth was accepted as a recorded variant")
	}
	unchanged, err := devices.Device(cfg.iface)
	must(err)
	foundForeign := false
	for _, current := range unchanged.Peers {
		if current.PublicKey == neighbor {
			foundForeign = receiveSourcesMatch(foreignGrowth.WGAllowedIPs, current.AllowedIPs)
		}
	}
	if !foundForeign {
		t.Fatal("rejected retirement changed unrecorded permissions")
	}
	must(retireReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{foreignGrowth}, []string{"10.77.0.1"}, allocated))
	replacement, replaced, _ := makeSender("replacement", "10.77.0.3", 51824)
	peer.PublicKey = replacement.String()
	peer.Endpoint = "127.0.0.1:51824"
	must(installReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer}, []string{"10.77.0.1"}))
	probe(replaced)
	// IPv6 uses the same authenticated underlay but has distinct kernel route
	// normalization and connected-subnet ownership. Keep the IPv4 stream active.
	addAddrs(t, link, "fd77::1/64")
	_, target, _ = net.ParseCIDR("fd77::1/128")
	listener6, err := net.ListenUDP("udp6", &net.UDPAddr{IP: target.IP, Port: 17777})
	must(err)
	defer listener6.Close()
	go func() {
		buffer := make([]byte, 128)
		for {
			n, remote, err := listener6.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			_, _ = listener6.WriteToUDP(buffer[:n], remote)
		}
	}()
	old6, sender6, _ := makeSender("old6", "fd77::3", 51825)
	peer6 := tunnel.PeerSpec{PublicKey: old6.String(), Remote: true, Endpoint: "127.0.0.1:51825", WGAllowedIPs: []string{"fd77::3/128"}, RouteHosts: []string{"fd77::3"}}
	own6 := []string{"10.77.0.1", "fd77::1"}
	allocated6 := []string{"10.77.0.1/24", "fd77::1/64"}
	must(installReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer6}, own6))
	probe(sender6)
	_, retired6, _ := net.ParseCIDR("fd77::3/128")
	foreign6 := netlink.Route{LinkIndex: link.Attrs().Index, Dst: retired6, Table: 999, Protocol: unix.RTPROT_STATIC}
	must(netlink.RouteAdd(&foreign6))
	if err := retireReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer6}, own6, allocated6); err == nil {
		t.Fatal("IPv6 retirement ignored a foreign route dependency")
	}
	probe(sender6)
	must(netlink.RouteDel(&foreign6))
	if err := retireReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer6}, own6, nil); err == nil {
		t.Fatal("IPv6 retirement adopted an unadmitted connected subnet")
	}
	probe(sender6)
	must(retireReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer6}, own6, allocated6))
	must(retireReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer6}, own6, allocated6))
	new6, replacement6, _ := makeSender("new6", "fd77::3", 51826)
	peer6.PublicKey, peer6.Endpoint = new6.String(), "127.0.0.1:51826"
	must(installReceiveNeighbors(context.Background(), devices, cfg, root, []tunnel.PeerSpec{peer6}, own6))
	probe(replacement6)
	if exchange(sender6) == nil {
		t.Fatal("retired IPv6 owner can still send traffic")
	}
	probe(replacement6)
}
