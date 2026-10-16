package main

import (
	"encoding/json"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// The packet experiment supplies already-authenticated devices. This adapter
// exercises the product route installer while the parent observes UDP traffic.
func TestNativeReceiveRouteSwitch(t *testing.T) {
	path := os.Getenv("CLDT_NATIVE_RECEIVE_ROUTE_SWITCH")
	if path == "" {
		t.Skip("requires isolated receiver packet experiment")
	}
	for _, kind := range []string{"net", "mnt"} {
		self, err := os.Readlink("/proc/self/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		init, err := os.Readlink("/proc/1/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		if self == init {
			t.Fatal("refusing guest host namespace")
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Destination string                        `json:"destination"`
		Selection   receiveRouteSelection         `json:"selection"`
		Plan        *tunnel.ReceiveTransitionPlan `json:"plan"`
		NodeUID     string                        `json:"nodeUID"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if request.Plan != nil {
		if !reflect.DeepEqual(request.Selection, receiveRouteSelection{}) {
			t.Fatal("route request has competing selection sources")
		}
		selections, err := tunnel.PlanReceiveRoutes(*request.Plan)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		request.Selection, found = selections[request.NodeUID][request.Destination]
		if !found {
			t.Fatal("committed plan does not select this node and destination")
		}
	}
	_, dst, err := net.ParseCIDR(request.Destination)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{iface: "wglegacy", routeTable: 518, fwmark: 518, receiveRoutes: map[string]receiveRouteSelection{request.Destination: request.Selection}}
	devices, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer devices.Close()
	before, err := devices.Device(request.Selection.Interface)
	if err != nil {
		t.Fatal(err)
	}
	if err := installRoutes(cfg, []net.IPNet{*dst}, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	link, err := netlink.LinkByName(request.Selection.Interface)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: 518}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, route := range routes {
		if route.Dst != nil && route.Dst.String() == dst.String() && route.LinkIndex == link.Attrs().Index {
			found = true
		}
	}
	if !found {
		t.Fatal("route did not select the prepared receiver")
	}
	after, err := devices.Device(request.Selection.Interface)
	if err != nil {
		t.Fatal(err)
	}
	if before.PrivateKey != after.PrivateKey || before.PublicKey != after.PublicKey || len(after.Peers) != 1 || before.Peers[0].PublicKey != after.Peers[0].PublicKey || !reflect.DeepEqual(before.Peers[0].AllowedIPs, after.Peers[0].AllowedIPs) {
		t.Fatal("route selection changed cryptographic receive ownership")
	}
	rules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"missing-route", "wrong-device", "wrong-source", "blackhole", "duplicate-route", "missing-rule", "missing-exemption"} {
		t.Run("readback/"+fault, func(t *testing.T) {
			var original netlink.Route
			for _, r := range routes {
				if r.Dst != nil && r.Dst.String() == dst.String() {
					original = r
					break
				}
			}
			if original.Dst == nil {
				t.Fatal("missing route fixture")
			}
			changed := original
			var removedRule *netlink.Rule
			switch fault {
			case "missing-route":
				if err := netlink.RouteDel(&original); err != nil {
					t.Fatal(err)
				}
			case "wrong-device":
				lo, err := netlink.LinkByName("lo")
				if err != nil {
					t.Fatal(err)
				}
				changed.LinkIndex = lo.Attrs().Index
				if err := netlink.RouteReplace(&changed); err != nil {
					t.Fatal(err)
				}
			case "wrong-source":
				for _, ip := range localIPs() {
					if ip.To4() != nil && ip.IsGlobalUnicast() {
						changed.Src = ip
						break
					}
				}
				if changed.Src == nil {
					t.Fatal("source-address test needs a local unicast address")
				}
				if err := netlink.RouteReplace(&changed); err != nil {
					t.Fatal(err)
				}
			case "blackhole":
				changed.LinkIndex = 0
				changed.Type = unix.RTN_BLACKHOLE
				changed.Scope = netlink.SCOPE_UNIVERSE
				if err := netlink.RouteReplace(&changed); err != nil {
					t.Fatal(err)
				}
			case "duplicate-route":
				changed.Priority = 31111
				if err := netlink.RouteAdd(&changed); err != nil {
					t.Fatal(err)
				}
			case "missing-rule", "missing-exemption":
				for _, r := range rules {
					wantTable, wantPriority, wantMark := cfg.routeTable, cfg.routeTable, uint32(0)
					if fault == "missing-exemption" {
						wantTable, wantPriority, wantMark = unix.RT_TABLE_MAIN, exemptRulePriority, uint32(cfg.fwmark)
					}
					if matchesTunnelRule(r, netlink.FAMILY_V4, wantTable, wantPriority, wantMark) {
						copy := r
						removedRule = &copy
						break
					}
				}
				if removedRule == nil {
					t.Fatal("missing policy fixture")
				}
				if err := netlink.RuleDel(removedRule); err != nil {
					t.Fatal(err)
				}
			}
			badRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
			if err != nil {
				t.Fatal(err)
			}
			badRules, err := netlink.RuleList(netlink.FAMILY_ALL)
			if err != nil {
				t.Fatal(err)
			}
			verifyErr := verifyReceiveRouteInstallation(cfg, []net.IPNet{*dst}, nil, nil)
			currentRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
			if err != nil {
				t.Fatal(err)
			}
			currentRules, err := netlink.RuleList(netlink.FAMILY_ALL)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(badRoutes, currentRoutes) || !reflect.DeepEqual(badRules, currentRules) {
				t.Fatal("readback mutated kernel state")
			}
			if removedRule != nil {
				if err := netlink.RuleAdd(removedRule); err != nil {
					t.Fatal(err)
				}
			} else {
				if fault == "duplicate-route" {
					if err := netlink.RouteDel(&changed); err != nil {
						t.Fatal(err)
					}
				}
				if err := netlink.RouteReplace(&original); err != nil {
					t.Fatal(err)
				}
			}
			if verifyErr == nil {
				t.Fatal("drifted route or policy passed readback")
			}
			if err := verifyReceiveRouteInstallation(cfg, []net.IPNet{*dst}, nil, nil); err != nil {
				t.Fatalf("restored state failed readback: %v", err)
			}
		})
	}
	t.Run("readback/shadowed-prefix", func(t *testing.T) {
		// Give this isolated fixture a declared /24 receiver permission so its
		// existing /32 route can exercise a more-specific shadow explicitly.
		broad := &net.IPNet{IP: dst.IP.Mask(net.CIDRMask(24, 32)), Mask: net.CIDRMask(24, 32)}
		selection := request.Selection
		selection.AllowedSources = []string{broad.String()}
		peer := before.Peers[0]
		if err := devices.ConfigureDevice(selection.Interface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: peer.PublicKey, ReplaceAllowedIPs: true, AllowedIPs: []net.IPNet{*broad}}}}); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := devices.ConfigureDevice(selection.Interface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: peer.PublicKey, ReplaceAllowedIPs: true, AllowedIPs: peer.AllowedIPs}}}); err != nil {
				t.Error(err)
			}
		}()
		broadCfg := cfg
		broadCfg.receiveRoutes = map[string]receiveRouteSelection{broad.String(): selection}
		route := netlink.Route{LinkIndex: link.Attrs().Index, Dst: broad, Table: cfg.routeTable, Scope: netlink.SCOPE_LINK}
		if err := netlink.RouteAdd(&route); err != nil {
			t.Fatal(err)
		}
		defer netlink.RouteDel(&route)
		beforeRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		err = verifyReceiveRouteInstallation(broadCfg, []net.IPNet{*broad}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "more-specific") {
			t.Fatalf("shadowed prefix accepted: %v", err)
		}
		afterRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
		if err != nil || !reflect.DeepEqual(beforeRoutes, afterRoutes) {
			t.Fatal("verification changed shadowed routes")
		}
	})
	for _, fault := range []string{"alias", "peer", "permission", "unknown-destination", "expanded-source", "unselected-destination", "unresolved-claim", "legacy-name-collision"} {
		broken := cfg
		selection := request.Selection
		destination := request.Destination
		hosts := []net.IPNet{*dst}
		var claimed []net.IPNet
		switch fault {
		case "alias":
			selection.Alias += "wrong"
		case "peer":
			selection.PeerPublicKey = selection.PublicKey
		case "permission":
			destination = "10.91.0.10/32"
			_, other, _ := net.ParseCIDR(destination)
			hosts = []net.IPNet{*other}
		case "expanded-source":
			_, extra, _ := net.ParseCIDR("0.0.0.0/0")
			expanded := append(append([]net.IPNet(nil), before.Peers[0].AllowedIPs...), *extra)
			if err := devices.ConfigureDevice(selection.Interface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: before.Peers[0].PublicKey, ReplaceAllowedIPs: true, AllowedIPs: expanded}}}); err != nil {
				t.Fatal(err)
			}
		case "legacy-name-collision":
			if _, err := netlink.LinkByName(cfg.iface); !isLinkNotFound(err) {
				continue
			}
			foreign := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: cfg.iface}}
			if err := netlink.LinkAdd(foreign); err != nil {
				t.Fatal(err)
			}
		case "unselected-destination", "unresolved-claim":
			// These cases apply after the surrounding experiment retires the
			// legacy device. They must fail before any routing mutation.
			if _, err := netlink.LinkByName(cfg.iface); !isLinkNotFound(err) {
				continue
			}
			_, other, _ := net.ParseCIDR("10.92.0.10/32")
			if fault == "unselected-destination" {
				hosts = append(hosts, *other)
			} else {
				claimed = []net.IPNet{*other}
			}
		case "unknown-destination":
			destination = "10.91.0.10/32"
		}
		broken.receiveRoutes = map[string]receiveRouteSelection{destination: selection}
		selectionErr := installRoutes(broken, hosts, claimed, nil, nil, nil, nil)
		if fault == "expanded-source" {
			if err := devices.ConfigureDevice(selection.Interface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: before.Peers[0].PublicKey, ReplaceAllowedIPs: true, AllowedIPs: before.Peers[0].AllowedIPs}}}); err != nil {
				t.Fatal(err)
			}
		}
		if fault == "legacy-name-collision" {
			foreign, err := netlink.LinkByName(cfg.iface)
			if err != nil || foreign.Type() != "dummy" {
				t.Fatalf("foreign interface was changed: %v", err)
			}
			if err := netlink.LinkDel(foreign); err != nil {
				t.Fatal(err)
			}
		}
		if selectionErr == nil {
			t.Fatalf("unsafe route selection accepted: %s", fault)
		}
		current, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: 518}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(routes, current) {
			t.Fatalf("rejected selection changed routes: %s", fault)
		}
		currentRules, err := netlink.RuleList(netlink.FAMILY_ALL)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(rules, currentRules) {
			t.Fatalf("rejected selection changed rules: %s", fault)
		}

	}
}

func TestReceiveSourcesMatchExactCommittedPermissions(t *testing.T) {
	expected := []string{"10.90.0.0/24", "2001:db8::/64"}
	for _, tc := range []struct {
		name     string
		prefixes []string
		want     bool
	}{
		{"reordered", []string{"2001:db8::/64", "10.90.0.0/24"}, true},
		{"broader", []string{"10.90.0.0/16", "2001:db8::/64"}, false},
		{"extra", []string{"10.90.0.0/24", "2001:db8::/64", "10.91.0.0/24"}, false},
		{"missing", []string{"10.90.0.0/24"}, false},
		{"duplicate", []string{"10.90.0.0/24", "10.90.0.0/24"}, false},
		{"default", []string{"0.0.0.0/0", "2001:db8::/64"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var actual []net.IPNet
			for _, raw := range tc.prefixes {
				_, p, err := net.ParseCIDR(raw)
				if err != nil {
					t.Fatal(err)
				}
				actual = append(actual, *p)
			}
			if got := receiveSourcesMatch(expected, actual); got != tc.want {
				t.Fatalf("permission match = %v, want %v", got, tc.want)
			}
			if receiveSourcesMatch(nil, actual) {
				t.Fatal("accepted missing permission binding")
			}
		})
	}
}
