package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

// The existing isolated packet experiment supplies real authenticated devices.
// Synthetic committed phases exercise the native backend only; API admission and
// receipt authentication have separate tests and remain required in production.
func TestNativeReceiveRoutePhase(t *testing.T) {
	path := os.Getenv("CLDT_NATIVE_RECEIVE_ROUTE_PHASE")
	if path == "" {
		t.Skip("requires isolated receiver packet experiment")
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
	var request struct {
		Plan    tunnel.ReceiveTransitionPlan `json:"plan"`
		KeyFile string                       `json:"keyFile"`
		Action  string                       `json:"action"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	keyRaw, err := os.ReadFile(request.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	key, err := wgtypes.ParseKey(strings.TrimSpace(string(keyRaw)))
	if err != nil {
		t.Fatal(err)
	}
	devices, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer devices.Close()
	plan := request.Plan
	raw, err = json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := tunnel.ReceiveTransitionSpec(plan, "native-cluster", fmt.Sprintf("%x", sha256.Sum256(raw)))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-5 * time.Second)
	tr, err := handover.NewTransition(spec, start, handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	advance := func(at time.Time) {
		view, err := tr.View()
		if err != nil {
			t.Fatal(err)
		}
		digest, err := view.Round.Digest()
		if err != nil {
			t.Fatal(err)
		}
		var receipts []handover.AuthenticatedReceipt
		for _, m := range view.Round.Members {
			receipts = append(receipts, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: digest, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: at}})
		}
		tr, err = tr.Advance(at, receipts)
		if err != nil {
			t.Fatal(err)
		}
	}
	advance(start.Add(time.Second))
	reverse := request.Action == "rollback" || request.Action == "rollback-drain"
	if reverse {
		tr, err = tr.Rollback(start.Add(2 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	if request.Action == "drain" || request.Action == "rollback-drain" {
		advance(start.Add(3 * time.Second))
	}
	if request.Action != "switch" && request.Action != "drain" && !reverse {
		t.Fatal("unsupported test action")
	}
	view, err := tr.View()
	if err != nil {
		t.Fatal(err)
	}
	receivers, err := tunnel.PlanReceiveRoutes(plan)
	if err != nil {
		t.Fatal(err)
	}
	intent := tunnel.ReceiveJournalRoutes{Plans: map[string]tunnel.ReceiveTransitionPlan{plan.MachineUID: plan}, Legacy: map[string]tunnel.ReceiveTransitionPlan{}, Receivers: receivers}
	if reverse {
		intent.Legacy[plan.MachineUID] = plan
		intent.Receivers = map[string]map[string]tunnel.ReceiveRouteSelection{}
	}
	cfg := config{iface: "wglegacy", nodeName: plan.CloudNodeName, routeTable: 518, fwmark: 518}
	// Protect an unrelated worker route in both the private and main tables.
	legacy, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := netlink.ParseAddr("203.0.113.77/32")
	if err != nil {
		t.Fatal(err)
	}
	unrelated := []netlink.Route{}
	for _, table := range []int{518, unix.RT_TABLE_MAIN} {
		route := netlink.Route{LinkIndex: legacy.Attrs().Index, Dst: addr.IPNet, Table: table, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT}
		if err := netlink.RouteReplace(&route); err != nil {
			t.Fatal(err)
		}
		unrelated = append(unrelated, route)
	}
	before, err := devices.Device(cfg.iface)
	if err != nil {
		t.Fatal(err)
	}
	if view.Round.Phase == handover.Switch {
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		for public, binding := range plan.Bindings {
			desired := false
			for _, peer := range plan.Desired.Peers {
				desired = desired || peer.PublicKey == public
			}
			if desired {
				continue
			}
			port := binding.ListenPort + 2000
			if err := devices.ConfigureDevice(binding.Interface, wgtypes.Config{ListenPort: &port}); err != nil {
				t.Fatal(err)
			}
			err := receiveRoutePhase(context.Background(), devices, cfg, key, nil, plan, view, plan.CloudNodeUID, intent)
			if err == nil || !strings.Contains(err.Error(), "listener differs") {
				t.Fatal("phase ignored changed retained receiver", err)
			}
			current, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
			if err != nil || !reflect.DeepEqual(routes, current) {
				t.Fatal("rejected retained receiver changed routes", err)
			}
			port = binding.ListenPort
			if err := devices.ConfigureDevice(binding.Interface, wgtypes.Config{ListenPort: &port}); err != nil {
				t.Fatal(err)
			}
		}
		var original *netlink.Route
		for _, route := range routes {
			if route.Dst != nil && route.Dst.String() == "10.90.0.10/32" {
				copy := route
				original = &copy
				break
			}
		}
		if original == nil {
			t.Fatal("missing route collision fixture")
		}
		if err := netlink.RouteDel(original); err != nil {
			t.Fatal(err)
		}
		foreign := netlink.Route{Dst: original.Dst, Table: cfg.routeTable, Type: unix.RTN_BLACKHOLE, Protocol: unix.RTPROT_STATIC}
		if err := netlink.RouteAdd(&foreign); err != nil {
			t.Fatal(err)
		}
		if err := receiveRoutePhase(context.Background(), devices, cfg, key, nil, plan, view, plan.CloudNodeUID, intent); err == nil {
			t.Fatal("switch overwrote foreign route collision")
		}
		current, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, route := range current {
			if route.Dst != nil && route.Dst.String() == original.Dst.String() && route.Type == unix.RTN_BLACKHOLE && route.Protocol == unix.RTPROT_STATIC {
				found = true
			}
		}
		if !found {
			t.Fatal("rejected switch modified foreign route")
		}
		if err := netlink.RouteDel(&foreign); err != nil {
			t.Fatal(err)
		}
		if err := netlink.RouteAdd(original); err != nil {
			t.Fatal(err)
		}
	}
	if err := receiveRoutePhase(context.Background(), devices, cfg, key, nil, plan, view, plan.CloudNodeUID, intent); err != nil {
		t.Fatal(err)
	}
	after, err := devices.Device(cfg.iface)
	if err != nil {
		t.Fatal(err)
	}
	if before.PrivateKey != after.PrivateKey || !samePeerConfiguration(before.Peers, after.Peers) {
		t.Fatal("route phase changed legacy receive permissions")
	}
	for _, want := range unrelated {
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: want.Table}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, got := range routes {
			if got.Dst != nil && got.Dst.String() == want.Dst.String() && got.LinkIndex == want.LinkIndex && got.Protocol == want.Protocol {
				found = true
			}
		}
		if !found {
			t.Fatal("route phase pruned unrelated worker", want.Table)
		}
	}
	// Drain observations cannot secretly repair a missing selected route.
	if view.Round.Phase == handover.Drain {
		dst, err := netlink.ParseAddr("10.90.0.10/32")
		if err != nil {
			t.Fatal(err)
		}
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		var removed *netlink.Route
		for _, route := range routes {
			if route.Dst != nil && route.Dst.String() == dst.IPNet.String() {
				copy := route
				removed = &copy
				break
			}
		}
		if removed == nil {
			t.Fatal("missing selected route fixture")
		}
		if err := netlink.RouteDel(removed); err != nil {
			t.Fatal(err)
		}
		if err := receiveRoutePhase(context.Background(), devices, cfg, key, nil, plan, view, plan.CloudNodeUID, intent); err == nil {
			t.Fatal("drain accepted missing selected route")
		}
		current, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		for _, route := range current {
			if route.Dst != nil && route.Dst.String() == dst.IPNet.String() {
				t.Fatal("drain repaired missing route")
			}
		}
		if err := netlink.RouteAdd(removed); err != nil {
			t.Fatal(err)
		}
	}
}

func samePeerConfiguration(a, b []wgtypes.Peer) bool {
	if len(a) != len(b) {
		return false
	}
	for _, first := range a {
		found := false
		for _, second := range b {
			if first.PublicKey == second.PublicKey && first.PresharedKey == second.PresharedKey && reflect.DeepEqual(first.AllowedIPs, second.AllowedIPs) && first.PersistentKeepaliveInterval == second.PersistentKeepaliveInterval {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
