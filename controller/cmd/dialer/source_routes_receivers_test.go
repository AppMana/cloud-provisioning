package main

import (
	"net"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestNativeTunnelSourceReceivers(t *testing.T) {
	if os.Getenv("CLDT_SOURCE_RECEIVERS_NETNS") != "1" {
		t.Skip("requires isolated guest network and mount namespaces")
	}
	for _, kind := range []string{"net", "mnt"} {
		self, err := os.Readlink("/proc/self/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		host, err := os.Readlink("/proc/1/ns/" + kind)
		if err != nil || self == host {
			t.Fatal("refusing guest host namespace")
		}
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		t.Fatal(err)
	}
	devices, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer devices.Close()
	legacy := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: "source-legacy"}}
	if err := netlink.LinkAdd(legacy); err != nil {
		t.Fatal(err)
	}
	defer netlink.LinkDel(legacy)
	if err := netlink.LinkSetUp(legacy); err != nil {
		t.Fatal(err)
	}
	cfg := config{iface: legacy.Attrs().Name, routeTable: 518, fwmark: 518, receiveRouteScope: map[string]bool{}, receiveSourceCandidates: map[string]receiveRouteSelection{}}
	addresses := []string{"10.88.0.1/24", "fd20::1/64"}
	var hosts []net.IPNet
	for _, raw := range []string{"10.88.0.128/32", "fd20::128/128"} {
		_, dst, _ := net.ParseCIDR(raw)
		hosts = append(hosts, *dst)
		cfg.receiveRouteScope[raw] = true
	}
	// Two independent receiver identities share the same source table.
	for i, host := range hosts {
		local, _ := wgtypes.GeneratePrivateKey()
		remote, _ := wgtypes.GeneratePrivateKey()
		names := []string{"src-client4", "src-server4"}
		if i == 1 {
			names = []string{"src-client6", "src-server6"}
		}
		keys := []wgtypes.Key{local, remote}
		for side, name := range names {
			link := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: name, Alias: "cloud-provisioning:receive-client:v1:" + name}}
			if err := netlink.LinkAdd(link); err != nil {
				t.Fatal(err)
			}
			defer netlink.LinkDel(link)
			if err := netlink.LinkSetAlias(link, "cloud-provisioning:receive-client:v1:"+name); err != nil {
				t.Fatal(err)
			}
			if err := netlink.LinkSetUp(link); err != nil {
				t.Fatal(err)
			}
			port, peerPort := 58120+i*2+side, 58120+i*2+1-side
			keepalive := time.Second
			peer := keys[1-side].PublicKey()
			mark := cfg.fwmark
			if err := devices.ConfigureDevice(name, wgtypes.Config{PrivateKey: &keys[side], ListenPort: &port, FirewallMark: &mark, Peers: []wgtypes.PeerConfig{{PublicKey: peer, Endpoint: &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: peerPort}, AllowedIPs: []net.IPNet{host}, PersistentKeepaliveInterval: &keepalive}}}); err != nil {
				t.Fatal(err)
			}
		}
		cfg.receiveSourceCandidates[host.String()] = receiveRouteSelection{Interface: names[0], Alias: "cloud-provisioning:receive-client:v1:" + names[0], PublicKey: local.PublicKey().String(), PeerPublicKey: remote.PublicKey().String(), AllowedSources: []string{host.String()}}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ready := true
		for _, selection := range cfg.receiveSourceCandidates {
			d, err := devices.Device(selection.Interface)
			if err != nil {
				t.Fatal(err)
			}
			ready = ready && len(d.Peers) == 1 && !d.Peers[0].LastHandshakeTime.IsZero()
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("receiver handshake timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	table, _ := tunnelSourceTable(cfg.routeTable)
	snapshot := func() []netlink.Route {
		t.Helper()
		_, routes, err := inspectConfiguredTunnelSources(cfg, table, legacy.Attrs().Index)
		if err != nil {
			t.Fatal(err)
		}
		return routes
	}
	for cycle := 0; cycle < 2; cycle++ {
		cfg.receiveSourceRoutes = nil
		if err := reconcileTunnelSourceRoutes(cfg, addresses, hosts); err != nil {
			t.Fatal(err)
		}
		cfg.receiveSourceRoutes = cfg.receiveSourceCandidates
		if err := verifyTunnelSourceRoutes(cfg, addresses, hosts); err == nil {
			t.Fatal("legacy routes satisfied receiver readback")
		}
		staged, selected, err := preflightReceiveSourceSwitchPlan(cfg, addresses, hosts, hosts)
		if err != nil {
			t.Fatal(err)
		}
		stagedCfg := cfg
		stagedCfg.receiveSourceRoutes = selected
		if err := reconcileTunnelSourceRoutes(stagedCfg, addresses, staged); err != nil {
			t.Fatal(err)
		}
		if err := verifyTunnelSourceRoutes(cfg, addresses, hosts); err != nil {
			t.Fatal(err)
		}
		for _, route := range snapshot() {
			link, err := netlink.LinkByName(cfg.receiveSourceCandidates[route.Dst.String()].Interface)
			if err != nil || route.LinkIndex != link.Attrs().Index {
				t.Fatal("source exception used wrong receiver", err)
			}
		}
		before := snapshot()
		wrong := before[0]
		other, err := netlink.LinkByName("src-client6")
		if err != nil {
			t.Fatal(err)
		}
		if wrong.LinkIndex == other.Attrs().Index {
			other, err = netlink.LinkByName("src-client4")
		}
		if err != nil {
			t.Fatal(err)
		}
		wrong.LinkIndex = other.Attrs().Index
		if err := netlink.RouteReplace(&wrong); err != nil {
			t.Fatal(err)
		}
		foreign, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		if err := reconcileTunnelSourceRoutes(cfg, addresses, hosts); err == nil {
			t.Fatal("another destination's receiver was accepted")
		}
		if _, _, err := preflightReceiveSourceSwitchPlan(cfg, addresses, hosts, hosts); err == nil {
			t.Fatal("switch accepted another destination's receiver")
		}
		after, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil || !reflect.DeepEqual(foreign, after) {
			t.Fatal("rejected receiver collision changed routes", err)
		}
		if err := netlink.RouteReplace(&before[0]); err != nil {
			t.Fatal(err)
		}
		// Withdrawal staging must keep current receivers until workload egress moves.
		cfg.receiveSourceRoutes = nil
		staged, selected, err = preflightReceiveSourceSwitchPlan(cfg, addresses, nil, hosts)
		if err != nil || len(selected) != 2 {
			t.Fatal("withdrawal staging lost current receivers", err)
		}
		stagedCfg = cfg
		stagedCfg.receiveSourceRoutes = selected
		if err := reconcileTunnelSourceRoutes(stagedCfg, addresses, staged); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, snapshot()) {
			t.Fatal("staging changed current source devices")
		}
		if err := reconcileTunnelSourceRoutes(cfg, addresses, nil); err != nil {
			t.Fatal(err)
		}
		if err := verifyTunnelSourceRoutes(cfg, addresses, nil); err != nil {
			t.Fatal(err)
		}
	}
	// A live candidate name with altered cryptographic identity grants no authority.
	cfg.receiveSourceRoutes = cfg.receiveSourceCandidates
	if err := reconcileTunnelSourceRoutes(cfg, addresses, hosts); err != nil {
		t.Fatal(err)
	}
	selection := cfg.receiveSourceCandidates[hosts[0].String()]
	link, _ := netlink.LinkByName(selection.Interface)
	if err := netlink.LinkSetAlias(link, "foreign"); err != nil {
		t.Fatal(err)
	}
	if err := reconcileTunnelSourceRoutes(cfg, addresses, hosts); err == nil {
		t.Fatal("foreign receiver accepted")
	}
	if err := verifyTunnelSourceRoutes(cfg, addresses, hosts); err == nil {
		t.Fatal("foreign receiver passed readback")
	}
	if _, _, err := preflightReceiveSourceSwitchPlan(cfg, addresses, hosts, hosts); err == nil {
		t.Fatal("foreign receiver passed switch preflight")
	}
	if err := netlink.LinkSetAlias(link, selection.Alias); err != nil {
		t.Fatal(err)
	}
	if err := verifyTunnelSourceRoutes(cfg, addresses, hosts); err != nil {
		t.Fatal("rejected operation changed source state", err)
	}
	cfg.receiveSourceRoutes = nil
	if err := reconcileTunnelSourceRoutes(cfg, addresses, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSourceReceiverCleanupRequiresScope(t *testing.T) {
	cfg := config{routeTable: 518, receiveSourceCandidates: map[string]receiveRouteSelection{"10.88.0.128/32": {Interface: "candidate"}}}
	if _, err := tunnelSourceReceiverIndices(cfg, nil); err == nil {
		t.Fatal("unscoped receiver candidate admitted")
	}
	if err := reconcileTunnelSourceRoutes(cfg, nil, nil); err == nil {
		t.Fatal("unscoped empty intent authorized cleanup")
	}
}
