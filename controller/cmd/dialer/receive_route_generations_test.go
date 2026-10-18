package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Native route admission and switching only. Loopback WireGuard handshakes
// authenticate both receivers, but no workload stream is measured. This does not
// qualify journal publication, whole-mesh continuity, reboot or permission
// retirement. Setup has a ten-second handshake deadline; rejection must leave
// routes unchanged. All links and policy live in disposable net/mount namespaces.
func TestNativeReceiveRouteGenerations(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_RECEIVE_GENERATIONS") != "1" {
		t.Skip("requires isolated guest net and mount namespaces")
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
	create := func(name, alias string, key wgtypes.Key) netlink.Link {
		t.Helper()
		link := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: name}}
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { netlink.LinkDel(link) })
		if alias != "" {
			if err := netlink.LinkSetAlias(link, alias); err != nil {
				t.Fatal(err)
			}
		}
		mark := 518
		if err := devices.ConfigureDevice(name, wgtypes.Config{PrivateKey: &key, FirewallMark: &mark}); err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
		return link
	}
	rootKey, _ := wgtypes.GeneratePrivateKey()
	root := create("genroot", "", rootKey)
	cfg := config{iface: root.Attrs().Name, fwmark: 518, routeTable: 518, receiveRouteScope: map[string]bool{"198.19.0.1/32": true, "2001:db8:19::1/128": true}}
	defer removeRouteRule(cfg.routeTable, cfg.fwmark)
	remoteKey, _ := wgtypes.GeneratePrivateKey()
	var generations []map[string]receiveRouteSelection
	var links []netlink.Link
	var hosts []net.IPNet
	for _, raw := range []string{"198.19.0.1/32", "2001:db8:19::1/128"} {
		_, dst, _ := net.ParseCIDR(raw)
		hosts = append(hosts, *dst)
	}
	for i := 0; i < 2; i++ {
		localKey, _ := wgtypes.GeneratePrivateKey()
		name, remoteName := fmt.Sprintf("genrx%d", i), fmt.Sprintf("gentx%d", i)
		alias := fmt.Sprintf("cloud-provisioning:receive:v1:native-generation-%d", i)
		links = append(links, create(name, alias, localKey))
		create(remoteName, "", remoteKey)
		local, err := devices.Device(name)
		if err != nil {
			t.Fatal(err)
		}
		remote, err := devices.Device(remoteName)
		if err != nil {
			t.Fatal(err)
		}
		keepalive := time.Second
		for _, peer := range []struct {
			name string
			key  wgtypes.Key
			port int
		}{{name, remoteKey.PublicKey(), remote.ListenPort}, {remoteName, localKey.PublicKey(), local.ListenPort}} {
			if err := devices.ConfigureDevice(peer.name, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: peer.key, Endpoint: &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: peer.port}, AllowedIPs: hosts, ReplaceAllowedIPs: true, PersistentKeepaliveInterval: &keepalive}}}); err != nil {
				t.Fatal(err)
			}
		}
		selection := receiveRouteSelection{Interface: name, Alias: alias, PublicKey: localKey.PublicKey().String(), PeerPublicKey: remoteKey.PublicKey().String(), AllowedSources: []string{"198.19.0.1/32", "2001:db8:19::1/128"}}
		generations = append(generations, map[string]receiveRouteSelection{hosts[0].String(): selection, hosts[1].String(): selection})
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ready := true
		for _, link := range links {
			device, err := devices.Device(link.Attrs().Name)
			if err != nil {
				t.Fatal(err)
			}
			ready = ready && len(device.Peers) == 1 && !device.Peers[0].LastHandshakeTime.IsZero()
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("receiver handshake deadline exceeded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, selected := range []int{0, 1, 0} {
		cfg.receiveRoutes = generations[selected]
		if err := preflightReceiveRouteGenerations(cfg, generations, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := installRoutes(cfg, hosts, nil, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := verifyReceiveRouteInstallation(cfg, hosts, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := preflightReceiveRouteGenerations(cfg, generations, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := preflightReceiveRouteGenerations(cfg, []map[string]receiveRouteSelection{generations[1-selected]}, nil, nil); err == nil {
			t.Fatal("unadmitted generation accepted")
		}
	}
	// A shared table can contain a first transition and a successor at once.
	// Root permission for one destination must not authorize another's route.
	for _, allowed := range []int{0, 1} {
		t.Run(fmt.Sprintf("mixed-root-permission-%d", allowed), func(t *testing.T) {
			rootDestinations := map[string]bool{hosts[allowed].String(): true}
			for _, dst := range hosts {
				route := netlink.Route{Dst: &dst, LinkIndex: root.Attrs().Index, Table: cfg.routeTable, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT, Priority: receiveRoutePriority(&dst)}
				if err := netlink.RouteReplace(&route); err != nil {
					t.Fatal(err)
				}
			}
			before, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
			if err != nil {
				t.Fatal(err)
			}
			if err := preflightReceiveRouteGenerations(cfg, generations, rootDestinations, nil); err == nil {
				t.Fatal("root permission escaped its destination")
			}
			after, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("rejection changed routes", err)
			}
			dst := hosts[1-allowed]
			route := netlink.Route{Dst: &dst, LinkIndex: links[0].Attrs().Index, Table: cfg.routeTable, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT, Priority: receiveRoutePriority(&dst)}
			if err := netlink.RouteReplace(&route); err != nil {
				t.Fatal(err)
			}
			if err := preflightReceiveRouteGenerations(cfg, generations, rootDestinations, nil); err != nil {
				t.Fatal("valid mixed graph rejected", err)
			}
			if err := installRoutes(cfg, hosts, nil, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, fault := range []string{"legacy-root", "foreign-source-permissions"} {
		t.Run(fault, func(t *testing.T) {
			admitted := generations
			if fault == "legacy-root" {
				for _, dst := range hosts {
					route := netlink.Route{Dst: &dst, LinkIndex: root.Attrs().Index, Table: cfg.routeTable, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT, Priority: receiveRoutePriority(&dst)}
					if err := netlink.RouteReplace(&route); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				bad := map[string]receiveRouteSelection{}
				for raw, selection := range generations[0] {
					selection.AllowedSources = []string{"198.19.0.1/32"}
					bad[raw] = selection
				}
				admitted = []map[string]receiveRouteSelection{bad, generations[1]}
			}
			before, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
			if err != nil {
				t.Fatal(err)
			}
			if err := preflightReceiveRouteGenerations(cfg, admitted, nil, nil); err == nil {
				t.Fatal("invalid predecessor route admitted")
			}
			after, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("rejection changed routes", err)
			}
			if err := installRoutes(cfg, hosts, nil, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Until the native executor understands both journal graphs, a different worker
// sharing the route table must not cause partial successor activation either.
func TestReceiveSuccessorNativeExecutionRemainsFenced(t *testing.T) {
	successor := tunnel.ReceiveTransitionPlan{PreviousReceiver: &tunnel.ReceivePreviousReceiver{}}
	for _, local := range []bool{false, true} {
		target := tunnel.ReceiveTransitionPlan{}
		if local {
			target = successor
		}
		err := reconcileReceiveLinuxPhase(context.Background(), nil, config{}, wgtypes.Key{}, nil, nil, target, handover.View{}, "", tunnel.ReceiveJournalRoutes{Plans: map[string]tunnel.ReceiveTransitionPlan{"successor": successor}}, false)
		if err == nil || !strings.Contains(err.Error(), "receiver-to-receiver") {
			t.Fatal("successor graph reached legacy native execution", err)
		}
	}
}

// Admission only: malformed root scope must fail before any kernel inspection;
// this does not establish forwarding or mixed-journal execution safety.
func TestReceiveRootDestinationsStayWithinScope(t *testing.T) {
	cfg := config{iface: "absent-interface", receiveRouteScope: map[string]bool{"198.19.0.1/32": true}}
	for _, allowed := range []map[string]bool{{"198.19.0.2/32": true}, {"198.19.0.1/32": false}} {
		err := preflightReceiveRouteGenerations(cfg, []map[string]receiveRouteSelection{{}}, allowed, nil)
		if err == nil || !strings.Contains(err.Error(), "legacy destination exceeds") {
			t.Fatal("malformed root admission reached kernel inspection", err)
		}
	}
}
