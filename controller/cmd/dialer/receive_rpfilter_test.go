package main

import (
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestNativeUnnumberedReceiverReversePathFiltering(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_RECEIVE_RPF") != "1" {
		t.Skip("requires isolated lab network and mount namespaces")
	}
	for _, kind := range []string{"net", "mnt"} {
		self, e := os.Readlink("/proc/self/ns/" + kind)
		if e != nil {
			t.Fatal(e)
		}
		init, e := os.Readlink("/proc/1/ns/" + kind)
		if e != nil || self == init {
			t.Fatal("refusing guest host namespace")
		}
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	parent, e := netns.Get()
	if e != nil {
		t.Fatal(e)
	}
	defer parent.Close()
	child, e := netns.New()
	if e != nil {
		t.Fatal(e)
	}
	defer child.Close()
	if e = netns.Set(parent); e != nil {
		t.Fatal(e)
	}
	must := func(e error) {
		t.Helper()
		if e != nil {
			t.Fatal(e)
		}
	}
	lo, e := netlink.LinkByName("lo")
	must(e)
	must(netlink.LinkSetUp(lo))
	owner := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "rpfowner"}}
	must(netlink.LinkAdd(owner))
	defer netlink.LinkDel(owner)
	addAddrs(t, owner, "192.0.2.10/32")
	must(netlink.LinkSetUp(owner))
	_, reverse, _ := net.ParseCIDR("10.77.0.2/32")
	must(netlink.RouteAdd(&netlink.Route{LinkIndex: owner.Attrs().Index, Dst: reverse, Scope: netlink.SCOPE_LINK}))
	root, _ := wgtypes.GeneratePrivateKey()
	sender, _ := wgtypes.GeneratePrivateKey()
	mesh := "rpf-test"
	doc := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: sender.PublicKey().String(), WGAllowedIPs: []string{"10.77.0.2/32"}}}}
	plan, e := tunnel.PlanReceiveHandover(mesh, doc, doc, nil)
	must(e)
	path := plan.Receivers[0]
	key := receiverKey(root, mesh, path.PublicKey)
	receiver := &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: path.Interface}, LinkType: "wireguard"}
	must(netlink.LinkAdd(receiver))
	defer netlink.LinkDel(receiver)
	must(netlink.LinkSetAlias(receiver, receiverAlias(mesh, path.PublicKey)))
	must(netlink.LinkSetUp(receiver))
	source := &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: "rpfsender"}, LinkType: "wireguard"}
	must(netlink.LinkAdd(source))
	// WireGuard's UDP socket stays in the namespace where the link was created.
	// Configure it before moving the link; loopback is the isolated underlay.
	devices, e := wgctrl.New()
	must(e)
	defer devices.Close()
	port, peerPort, mark := 51821, 51822, 517
	must(devices.ConfigureDevice(path.Interface, wgtypes.Config{PrivateKey: &key, ListenPort: &port, FirewallMark: &mark, Peers: []wgtypes.PeerConfig{{PublicKey: sender.PublicKey(), AllowedIPs: []net.IPNet{*reverse}}}}))
	_, target, _ := net.ParseCIDR("192.0.2.10/32")
	must(devices.ConfigureDevice("rpfsender", wgtypes.Config{PrivateKey: &sender, ListenPort: &peerPort, Peers: []wgtypes.PeerConfig{{PublicKey: key.PublicKey(), Endpoint: &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}, AllowedIPs: []net.IPNet{*target}}}}))
	must(netlink.LinkSetNsFd(source, int(child)))
	var sending *net.UDPConn
	inNamespace(t, child, parent, func() {
		link, e := netlink.LinkByName("rpfsender")
		must(e)
		addAddrs(t, link, "10.77.0.2/32")
		must(netlink.LinkSetUp(link))
		must(netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: target, Scope: netlink.SCOPE_LINK}))
		sending, e = net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP("10.77.0.2")}, &net.UDPAddr{IP: target.IP, Port: 17777})
		must(e)
	})
	defer sending.Close()
	listener, e := net.ListenUDP("udp4", &net.UDPAddr{IP: target.IP, Port: 17777})
	must(e)
	defer listener.Close()
	for _, name := range []string{"all", "default", "lo", "rpfowner", path.Interface} {
		must(setSysctl("ipv4/conf/"+name+"/rp_filter", "2"))
	}
	// These devices currently rely on the global floor. Removing it must
	// preserve their effective policy, not simply leave their local values.
	must(setSysctl("ipv4/conf/default/rp_filter", "0"))
	must(setSysctl("ipv4/conf/lo/rp_filter", "1"))
	must(setSysctl("ipv4/conf/rpfowner/rp_filter", "0"))
	receive := func(want int) {
		t.Helper()
		for i := 0; i < 10; i++ {
			_, e := sending.Write([]byte("probe"))
			must(e)
			time.Sleep(20 * time.Millisecond)
		}
		must(listener.SetReadDeadline(time.Now().Add(300 * time.Millisecond)))
		count := 0
		for {
			b := make([]byte, 64)
			_, _, e := listener.ReadFromUDP(b)
			if e != nil {
				if x, ok := e.(net.Error); !ok || !x.Timeout() {
					t.Fatal(e)
				}
				break
			}
			count++
		}
		if count != want {
			t.Fatalf("received %d/10, want %d", count, want)
		}
	}
	receive(0)
	live, e := devices.Device(path.Interface)
	must(e)
	if live.Peers[0].LastHandshakeTime.IsZero() {
		t.Fatal("loss was not reproduced over an authenticated tunnel")
	}
	if e := ensureReceiveReversePathFiltering(path.Interface, "foreign-owner"); e == nil {
		t.Fatal("foreign ownership changed RPF")
	}
	global, e := readSysctl("ipv4/conf/all/rp_filter")
	must(e)
	if global != "2" {
		t.Fatal("failed ownership check changed global policy")
	}
	// Exercise the production preparation path, including kernel readback.
	_, e = prepareReceivePaths(devices, mesh, root, doc, doc, nil, 1420, mark, map[string]int{path.PublicKey: port})
	must(e)
	receive(10)
	for _, name := range []string{"default", "lo", "rpfowner"} {
		v, e := readSysctl("ipv4/conf/" + name + "/rp_filter")
		must(e)
		if v != "2" {
			t.Fatalf("changed unrelated policy on %s: %s", name, v)
		}
	}
	addrs, e := netlink.AddrList(receiver, netlink.FAMILY_V4)
	must(e)
	if len(addrs) != 0 {
		t.Fatal("preparation numbered the receiver")
	}
	must(verifyReceiveReversePathFiltering(path.Interface))
	must(setSysctl("ipv4/conf/all/rp_filter", "2"))
	if e := verifyReceiveReversePathFiltering(path.Interface); e == nil {
		t.Fatal("readback accepted restored global filtering")
	}
	_, e = prepareReceivePaths(devices, mesh, root, doc, doc, nil, 1420, mark, map[string]int{path.PublicKey: port})
	must(e)
	receive(10)
	// Unauthorized inner sources still fail WireGuard's source-key check.
	inNamespace(t, child, parent, func() {
		link, e := netlink.LinkByName("rpfsender")
		must(e)
		addAddrs(t, link, "10.77.0.3/32")
		bad, e := net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP("10.77.0.3")}, &net.UDPAddr{IP: target.IP, Port: 17777})
		must(e)
		defer bad.Close()
		_, e = bad.Write([]byte("unauthorized"))
		must(e)
	})
	must(listener.SetReadDeadline(time.Now().Add(300 * time.Millisecond)))
	if _, _, e = listener.ReadFromUDP(make([]byte, 64)); e == nil {
		t.Fatal("unauthorized inner source delivered")
	}
}
