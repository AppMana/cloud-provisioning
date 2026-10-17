package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// This checks eventual device restoration, not the deployment recovery deadline.
// Idle WireGuard rekey latency must be qualified separately with workload traffic.
func TestNativeRestoreCompletedReceivePaths(t *testing.T) {
	if os.Getenv("CLDT_RESTORE_RECEIVE_PATHS") != "1" {
		t.Skip("requires isolated VM mount and network namespaces")
	}
	requireStableAddressNamespace(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	lo, err := netlink.LinkByName("lo")
	must(err)
	must(netlink.LinkSetUp(lo))
	address, err := netlink.ParseAddr("198.51.100.1/32")
	must(err)
	must(netlink.AddrAdd(lo, address))
	devices, err := wgctrl.New()
	must(err)
	defer devices.Close()
	root, err := wgtypes.GeneratePrivateKey()
	must(err)
	site, err := wgtypes.GeneratePrivateKey()
	must(err)
	pub := site.PublicKey().String()
	peers := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: pub, WGAllowedIPs: []string{"10.90.0.0/24", "fd90::/64"}}}}
	base, err := json.Marshal(peers)
	must(err)
	preparation, err := tunnel.PlanReceiveHandover("restore-mesh", peers, peers, nil)
	must(err)
	binding := tunnel.ReceiveBinding{Interface: preparation.Receivers[0].Interface, PeerPublicKey: pub, PublicKey: receiverKey(root, "restore-mesh", pub).PublicKey().String(), ListenPort: 51821}
	siteDoc := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: root.PublicKey().String(), Remote: true, Endpoint: "198.51.100.1:51820", RouteHosts: []string{"172.29.0.1", "fd29::1"}, WGAllowedIPs: []string{"172.29.0.1/32", "fd29::1/128"}}}}
	client, err := tunnel.PlanSiteReceiveClient("restore-mesh", pub, root.PublicKey().String(), binding, siteDoc, siteDoc)
	must(err)
	plan := tunnel.ReceiveTransitionPlan{Version: 1, Namespace: "test", MeshName: "mesh", MeshUID: "restore-mesh", MachineName: "worker", MachineUID: "worker-uid", AdoptionName: tunnel.AdoptionSecretName("worker"), AdoptionUID: "adoption-uid", CloudNodeName: "cloud", CloudNodeUID: "cloud-uid", RootPublicKey: root.PublicKey().String(), Base: base, Native: []netip.Prefix{}, Desired: peers, SiteDesired: siteDoc, Ports: map[string]int{pub: 51821}, Bindings: map[string]tunnel.ReceiveBinding{pub: binding}, Sites: map[string]tunnel.ReceiveTransitionSite{pub: {NodeName: "site", NodeUID: "site-uid", Client: client}}}
	plan.CloudAddresses = []string{"172.29.0.1/32", "fd29::1/128"}
	local := plan.Sites[pub]
	local.RouteSources = []string{"10.90.0.2", "fd90::2"}
	local.Legacy = &tunnel.ReceiveSiteLegacy{Sources: []string{"10.90.0.0/24", "10.90.0.2/32", "fd90::/64", "fd90::2/128"}, PeerPublicKey: pub, Tunnel: &tunnel.ReceiveSiteTunnel{Addresses: []string{"10.90.0.2/24", "fd90::2/64"}, Hosts: []string{}}}
	forward := *local.Legacy
	local.Forward = &forward
	plan.Sites[pub] = local
	raw, err := json.Marshal(plan)
	must(err)
	spec, err := tunnel.ReceiveTransitionSpec(plan, "cluster", fmt.Sprintf("%x", sha256.Sum256(raw)))
	must(err)
	spec.Phase = handover.Retire
	view := handover.View{Round: handover.Round{Spec: spec}, Complete: true, ServingGeneration: spec.ToGeneration}
	intent := tunnel.ReceiveJournalRoutes{Plans: map[string]tunnel.ReceiveTransitionPlan{plan.MachineUID: plan}, Views: map[string]handover.View{plan.MachineUID: view}, Legacy: map[string]tunnel.ReceiveTransitionPlan{}}
	intent.Receivers, err = tunnel.PlanReceiveRoutes(plan)
	must(err)
	intent.Sources, err = tunnel.PlanReceiveSourceRoutes(intent)
	must(err)
	cfg := config{iface: "legacy", nodeName: "cloud", mtu: 1400, fwmark: 518, routeTable: 518}
	restore := func() error {
		return restoreCompletedReceiveState(context.Background(), devices, cfg, root, plan, view, "cloud-uid", intent)
	}
	wait := func(action func() error, limits ...time.Duration) {
		t.Helper()
		started := time.Now()
		limit := 10 * time.Second
		if len(limits) != 0 {
			limit = limits[0]
		}
		deadline := started.Add(limit)
		for {
			err := action()
			if err == nil {
				t.Logf("authenticated path restored after %s", time.Since(started))
				return
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if err := restore(); err == nil {
		t.Fatal("receiver without fresh handshake was accepted")
	}
	_, err = prepareSiteReceiveClient(devices, plan.MeshUID, site, plan.RootPublicKey, binding, siteDoc, siteDoc, 1400, 518)
	must(err)
	wait(restore)
	siteCfg := cfg
	siteCfg.nodeName = "site"
	siteCfg.iface = "sitelegacy"
	restoreSite := func() error {
		return restoreCompletedReceiveState(context.Background(), devices, siteCfg, site, plan, view, "site-uid", intent)
	}
	wait(restoreSite)
	// Recreated devices have new indices. Compare route identity by device name
	// while retaining every selector and attribute in the kernel route record.
	routeSnapshot := func() []string {
		t.Helper()
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
		must(err)
		var result []string
		for _, route := range routes {
			name := ""
			if route.LinkIndex != 0 {
				link, err := netlink.LinkByIndex(route.LinkIndex)
				must(err)
				name = link.Attrs().Name
			}
			route.LinkIndex = 0
			raw, err := json.Marshal(route)
			must(err)
			result = append(result, name+string(raw))
		}
		sort.Strings(result)
		return result
	}
	// The transport/root/address callback cannot authorize shared policy repair.
	if err := verifyReceivePolicyRules(cfg); err == nil {
		t.Fatal("maintenance installed shared policy rules")
	}
	must(ensureRouteRule(cfg.routeTable, cfg.fwmark))
	verify := func() error {
		return receiveLinuxPhase(context.Background(), devices, cfg, root, nil, plan.CloudAddresses, plan, view, "cloud-uid", intent)
	}
	must(verify())
	owned, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
	must(err)
	if len(owned) == 0 {
		t.Fatal("fixture has no selected routes")
	}
	original := owned[0]
	foreignRoute := original
	foreignRoute.Protocol = unix.RTPROT_STATIC
	must(netlink.RouteReplace(&foreignRoute))
	conflict := routeSnapshot()
	if err := restore(); err == nil {
		t.Fatal("maintenance adopted conflicting route ownership")
	}
	if !reflect.DeepEqual(conflict, routeSnapshot()) {
		t.Fatal("rejected maintenance changed routes")
	}
	must(netlink.RouteDel(&foreignRoute))
	if err := verify(); err == nil {
		t.Fatal("readback accepted missing route")
	}
	wait(restore)
	must(verify())
	// This route lies outside all journal destinations and must survive repair.
	protected := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "protected"}}
	must(netlink.LinkAdd(protected))
	must(netlink.LinkSetUp(protected))
	destination, err := netlink.ParseIPNet("192.0.2.42/32")
	must(err)
	must(netlink.RouteAdd(&netlink.Route{LinkIndex: protected.Attrs().Index, Table: cfg.routeTable, Dst: destination, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_STATIC}))
	routes := routeSnapshot()
	rules, err := netlink.RuleList(netlink.FAMILY_ALL)
	must(err)
	// Lose each selected device in turn while the other endpoint remains live.
	cloudLink, err := netlink.LinkByName(binding.Interface)
	must(err)
	must(netlink.LinkDel(cloudLink))
	wait(restore, 3*time.Minute)
	siteLink, err := netlink.LinkByName(client.Interface)
	must(err)
	must(netlink.LinkDel(siteLink))
	wait(restoreSite)
	wait(restore)
	rootDevice, err := devices.Device(cfg.iface)
	must(err)
	if rootDevice.PublicKey != root.PublicKey() || len(rootDevice.Peers) != 0 {
		t.Fatal("restoration changed root identity or recreated retired peers")
	}
	afterRoutes := routeSnapshot()
	afterRules, err := netlink.RuleList(netlink.FAMILY_ALL)
	must(err)
	if !reflect.DeepEqual(routes, afterRoutes) || !reflect.DeepEqual(rules, afterRules) {
		t.Fatal("transport restoration changed routes or policy")
	}
	cloudLink, err = netlink.LinkByName(binding.Interface)
	must(err)
	must(netlink.LinkDel(cloudLink))
	// Rollback selection must never revive the now-retired receiver.
	view.RollbackRequested = true
	intent.Views[plan.MachineUID] = view
	intent.Legacy[plan.MachineUID] = plan
	must(restore())
	if _, err := netlink.LinkByName(binding.Interface); !isLinkNotFound(err) {
		t.Fatal("rollback resurrected receiver", err)
	}
	view.RollbackRequested = false
	intent.Views[plan.MachineUID] = view
	delete(intent.Legacy, plan.MachineUID)
	attrs := netlink.NewLinkAttrs()
	attrs.Name = binding.Interface
	must(netlink.LinkAdd(&netlink.Dummy{LinkAttrs: attrs}))
	if err := restore(); err == nil {
		t.Fatal("restoration adopted foreign device")
	}
	foreign, err := netlink.LinkByName(binding.Interface)
	must(err)
	if foreign.Type() != "dummy" {
		t.Fatal("foreign device replaced")
	}
}
