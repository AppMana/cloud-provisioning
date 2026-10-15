package main

import (
	"encoding/json"
	"net"
	"os"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
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
		if request.Selection != (receiveRouteSelection{}) {
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
	for _, fault := range []string{"alias", "peer", "permission", "unknown-destination"} {
		broken := cfg
		selection := request.Selection
		destination := request.Destination
		hosts := []net.IPNet{*dst}
		switch fault {
		case "alias":
			selection.Alias += "wrong"
		case "peer":
			selection.PeerPublicKey = selection.PublicKey
		case "permission":
			destination = "10.91.0.10/32"
			_, other, _ := net.ParseCIDR(destination)
			hosts = []net.IPNet{*other}
		case "unknown-destination":
			destination = "10.91.0.10/32"
		}
		broken.receiveRoutes = map[string]receiveRouteSelection{destination: selection}
		if err := installRoutes(broken, hosts, nil, nil, nil, nil, nil); err == nil {
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
