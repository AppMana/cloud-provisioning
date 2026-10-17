package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
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
	siteDoc := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: root.PublicKey().String(), Remote: true, Endpoint: "198.51.100.1:51820", WGAllowedIPs: []string{"172.29.0.1/32", "fd29::1/128"}}}}
	client, err := tunnel.PlanSiteReceiveClient("restore-mesh", pub, root.PublicKey().String(), binding, siteDoc, siteDoc)
	must(err)
	plan := tunnel.ReceiveTransitionPlan{Version: 1, Namespace: "test", MeshName: "mesh", MeshUID: "restore-mesh", MachineName: "worker", MachineUID: "worker-uid", AdoptionName: tunnel.AdoptionSecretName("worker"), AdoptionUID: "adoption-uid", CloudNodeName: "cloud", CloudNodeUID: "cloud-uid", RootPublicKey: root.PublicKey().String(), Base: base, Native: []netip.Prefix{}, Desired: peers, SiteDesired: siteDoc, Ports: map[string]int{pub: 51821}, Bindings: map[string]tunnel.ReceiveBinding{pub: binding}, Sites: map[string]tunnel.ReceiveTransitionSite{pub: {NodeName: "site", NodeUID: "site-uid", Client: client}}}
	raw, err := json.Marshal(plan)
	must(err)
	spec, err := tunnel.ReceiveTransitionSpec(plan, "cluster", fmt.Sprintf("%x", sha256.Sum256(raw)))
	must(err)
	spec.Phase = handover.Retire
	view := handover.View{Round: handover.Round{Spec: spec}, Complete: true, ServingGeneration: spec.ToGeneration}
	intent := tunnel.ReceiveJournalRoutes{Plans: map[string]tunnel.ReceiveTransitionPlan{plan.MachineUID: plan}, Views: map[string]handover.View{plan.MachineUID: view}, Legacy: map[string]tunnel.ReceiveTransitionPlan{}}
	cfg := config{iface: "legacy", nodeName: "cloud", mtu: 1400, fwmark: 518}
	restore := func() error {
		return restoreCompletedReceivePaths(context.Background(), devices, cfg, root, plan, view, "cloud-uid", intent)
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
	must(err)
	rules, err := netlink.RuleList(netlink.FAMILY_ALL)
	must(err)
	wait := func(action func() error) {
		t.Helper()
		started := time.Now()
		deadline := started.Add(3 * time.Minute)
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
	restoreSite := func() error {
		return restoreCompletedReceivePaths(context.Background(), devices, siteCfg, site, plan, view, "site-uid", intent)
	}
	wait(restoreSite)
	// Lose each selected device in turn while the other endpoint remains live.
	cloudLink, err := netlink.LinkByName(binding.Interface)
	must(err)
	must(netlink.LinkDel(cloudLink))
	wait(restore)
	siteLink, err := netlink.LinkByName(client.Interface)
	must(err)
	must(netlink.LinkDel(siteLink))
	wait(restoreSite)
	wait(restore)
	if _, err := netlink.LinkByName(cfg.iface); !isLinkNotFound(err) {
		t.Fatal("restoration recreated legacy root", err)
	}
	afterRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
	must(err)
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
