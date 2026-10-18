package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

// Native phase execution with authenticated loopback peers. Run each role and
// outcome in fresh isolated net/mount namespaces. Synthetic views do not prove
// API admission, relay ordering, workload continuity, reboot, source exceptions
// or live allocation churn. Retirement must reject a foreign route dependency
// without deleting its device or route. IPv6 readback is not traffic evidence.
// Handshake deadline: ten seconds. Namespace teardown is fixture cleanup,
// not autonomous rollback. Completed-state recovery restores a deliberately
// deleted IPv4 route; it does not model a reboot. The caller must bound runtime.
func TestNativeReceiveSuccessorPhases(t *testing.T) {
	role, outcome := os.Getenv("CLDT_NATIVE_SUCCESSOR_ROLE"), os.Getenv("CLDT_NATIVE_SUCCESSOR_OUTCOME")
	if role == "" {
		t.Skip("requires isolated guest namespaces")
	}
	if (role != "cloud" && role != "site") || (outcome != "forward" && outcome != "rollback" && outcome != "abort") {
		t.Fatal("invalid scenario")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"net", "mnt"} {
		self, err := os.Readlink("/proc/self/ns/" + kind)
		must(err)
		host, err := os.Readlink("/proc/1/ns/" + kind)
		must(err)
		if self == host {
			t.Fatal("refusing guest host namespace")
		}
	}
	lo, err := netlink.LinkByName("lo")
	must(err)
	must(netlink.LinkSetUp(lo))
	endpoint, err := netlink.ParseAddr("198.18.0.1/32")
	must(err)
	must(netlink.AddrAdd(lo, endpoint))
	devices, err := wgctrl.New()
	must(err)
	defer devices.Close()
	root, err := wgtypes.GeneratePrivateKey()
	must(err)
	siteKey, err := wgtypes.GeneratePrivateKey()
	must(err)
	public := siteKey.PublicKey().String()
	base := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: public, WGAllowedIPs: []string{"198.19.0.1/32", "2001:db8:19::1/128"}}}}
	baseRaw, err := json.Marshal(base)
	must(err)
	cloud := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: root.PublicKey().String(), Remote: true, RouteHosts: []string{"10.88.0.3", "2001:db8:88::3"}, Endpoint: "198.18.0.1:51820", WGAllowedIPs: []string{"10.88.0.3/32", "2001:db8:88::3/128"}}}}
	makePlan := func(generation string, port int) tunnel.ReceiveTransitionPlan {
		p := tunnel.ReceiveTransitionPlan{Version: 1, Namespace: "test", MeshName: "mesh", MeshUID: "mesh", MachineUID: "worker", CloudNodeName: "cloud", CloudNodeUID: "cloud", CloudAddresses: []string{"10.88.0.3/32", "2001:db8:88::3/128"}, RootPublicKey: root.PublicKey().String(), ReceiverGeneration: generation, Base: baseRaw, Desired: base, SiteDesired: cloud, Native: []netip.Prefix{}}
		scope, err := p.ReceiveScope()
		must(err)
		prep, err := tunnel.PlanReceiveHandover(scope, base, base, nil)
		must(err)
		binding := tunnel.ReceiveBinding{Interface: prep.Receivers[0].Interface, PeerPublicKey: public, PublicKey: receiverKey(root, scope, public).PublicKey().String(), ListenPort: port}
		client, err := tunnel.PlanSiteReceiveClient(scope, public, p.RootPublicKey, binding, cloud, cloud)
		must(err)
		p.Bindings = map[string]tunnel.ReceiveBinding{public: binding}
		p.Ports = map[string]int{public: port}
		path := &tunnel.ReceiveSitePath{Sources: []string{"198.19.0.1/32", "2001:db8:19::1/128"}, PeerPublicKey: public, Tunnel: &tunnel.ReceiveSiteTunnel{Addresses: []string{"198.19.0.1/32", "2001:db8:19::1/128"}, Hosts: []string{}}}
		p.Sites = map[string]tunnel.ReceiveTransitionSite{public: {NodeUID: "site", NodeName: "site", RouteSources: []string{"198.19.0.1", "2001:db8:19::1"}, Client: client, Legacy: path, Forward: path}}
		return p
	}
	old, next := makePlan(strings.Repeat("a", 64), 58220), makePlan(strings.Repeat("b", 64), 58221)
	next.Version = 2
	next.SiteBase = &old.SiteDesired
	next.PreviousReceiver = &tunnel.ReceivePreviousReceiver{Generation: "receivers:" + strings.Repeat("c", 64), Plan: &old}
	_, err = tunnel.PlanReceiveRoutes(next)
	must(err)
	key, allocated := root, []string{"10.88.0.3/32", "2001:db8:88::3/128"}
	if role == "site" {
		key, allocated = siteKey, []string{"198.19.0.1/32", "2001:db8:19::1/128"}
	}
	own, bound, err := tunnel.ReceiveLocalNetwork(next, role)
	must(err)
	allocated = bound
	cfg := config{iface: "succroot", nodeName: role, fwmark: 518, routeTable: 518, mtu: 1400}
	link := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: cfg.iface}}
	must(netlink.LinkAdd(link))
	mark := cfg.fwmark
	must(devices.ConfigureDevice(cfg.iface, wgtypes.Config{PrivateKey: &key, FirewallMark: &mark}))
	must(netlink.LinkSetUp(link))
	for _, raw := range allocated {
		addr, err := netlink.ParseAddr(raw)
		must(err)
		must(netlink.AddrAdd(link, addr))
	}
	// Prepare authenticated endpoint fixtures. Phase calls below verify and
	// reprepare local paths through the production executor.
	for _, p := range []tunnel.ReceiveTransitionPlan{old, next} {
		must(prepareReceivePhasePaths(devices, cfg, root, p, "cloud"))
		must(prepareReceivePhasePaths(devices, cfg, siteKey, p, "site"))
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = verifyReceivePhasePaths(devices, cfg, key, old, role)
		if err == nil {
			err = verifyReceivePhasePaths(devices, cfg, key, next, role)
		}
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("handshake deadline: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	oldRoutes, err := tunnel.PlanReceiveRoutes(old)
	must(err)
	initial := cfg
	initial.receiveRoutes = oldRoutes[role]
	initial.receiveRouteScope = map[string]bool{}
	var hosts []net.IPNet
	for raw := range oldRoutes[role] {
		_, dst, err := net.ParseCIDR(raw)
		must(err)
		hosts = append(hosts, *dst)
		initial.receiveRouteScope[raw] = true
	}
	must(installRoutes(initial, hosts, nil, nil, nil, nil, own))
	raw, err := json.Marshal(next)
	must(err)
	spec, err := tunnel.ReceiveTransitionSpec(next, "native-cluster", fmt.Sprintf("%x", sha256.Sum256(raw)))
	must(err)
	tr, err := handover.NewTransition(spec, time.Now(), handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
	must(err)
	rolled := false
	for {
		view, err := tr.View()
		must(err)
		active, rootSelected, err := next.SelectedReceiveTransport(view)
		must(err)
		if rootSelected {
			t.Fatal("selected root")
		}
		routes, err := tunnel.PlanReceiveRoutes(active)
		must(err)
		intent := tunnel.ReceiveJournalRoutes{Views: map[string]handover.View{"worker": view}, Plans: map[string]tunnel.ReceiveTransitionPlan{"worker": next}, Legacy: map[string]tunnel.ReceiveTransitionPlan{}, Receivers: routes}
		intent.Sources, err = tunnel.PlanReceiveSourceRoutes(intent)
		must(err)
		t.Logf("role=%s outcome=%s phase=%s rollback=%v complete=%v", role, outcome, view.Round.Phase, view.RollbackRequested, view.Complete)
		if !view.Complete && (view.Round.Phase == handover.Retire || view.Round.Phase == handover.Abort) {
			retired := old
			if outcome != "forward" {
				retired = next
			}
			selections, err := receivePhaseSelections(key, retired, role)
			must(err)
			link, err := netlink.LinkByName(selections[0].Interface)
			must(err)
			_, dst, err := net.ParseCIDR("203.0.113.99/32")
			must(err)
			foreign := netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst, Table: 600, Scope: netlink.SCOPE_LINK}
			must(netlink.RouteAdd(&foreign))
			if err := receiveLinuxPhase(context.Background(), devices, cfg, key, own, allocated, next, view, role, intent); err == nil {
				t.Fatal("retired a device with a foreign route dependency")
			}
			_, err = netlink.LinkByName(selections[0].Interface)
			must(err)
			remaining, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: 600}, netlink.RT_FILTER_TABLE)
			must(err)
			if len(remaining) != 1 || remaining[0].LinkIndex != foreign.LinkIndex || remaining[0].Dst.String() != dst.String() {
				t.Fatal("retirement rejection changed foreign route")
			}
			must(netlink.RouteDel(&foreign))
		}
		must(receiveLinuxPhase(context.Background(), devices, cfg, key, own, allocated, next, view, role, intent))
		must(receiveLinuxPhase(context.Background(), devices, cfg, key, own, allocated, next, view, role, intent))
		if view.Complete {
			installed, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
			must(err)
			if len(installed) == 0 {
				t.Fatal("no selected route to interrupt")
			}
			must(netlink.RouteDel(&installed[0]))
			must(restoreCompletedReceiveRoutes(context.Background(), devices, cfg, key, own, allocated, next, view, role, intent))
			must(receiveLinuxPhase(context.Background(), devices, cfg, key, own, allocated, next, view, role, intent))
			break
		}
		at := view.Round.IssuedAt.Add(time.Second)
		if view.Round.Phase == handover.Drain {
			at = view.DrainUntil
		}
		if !rolled && ((outcome == "abort" && view.Round.Phase == handover.Prepare) || (outcome == "rollback" && view.Round.Phase == handover.Switch)) {
			tr, err = tr.Rollback(at)
			must(err)
			rolled = true
			continue
		}
		digest, err := view.Round.Digest()
		must(err)
		var receipts []handover.AuthenticatedReceipt
		for _, m := range view.Round.Members {
			receipts = append(receipts, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{NodeUID: m.NodeUID, DeviceID: m.DeviceID, RoundHash: digest, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: at}})
		}
		tr, err = tr.Advance(at, receipts)
		must(err)
	}
}
