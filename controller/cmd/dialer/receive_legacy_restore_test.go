package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestSelectedLegacyPeersHonorJournalSelection(t *testing.T) {
	root, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	site, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	plan := receiveRestoreTestPlan(t, root, site)
	view := handover.View{Complete: true, ServingGeneration: "legacy:" + tunnel.HashPeerList(plan.Base)}
	intent := tunnel.ReceiveJournalRoutes{Views: map[string]handover.View{plan.MachineUID: view}, Plans: map[string]tunnel.ReceiveTransitionPlan{plan.MachineUID: plan}, Legacy: map[string]tunnel.ReceiveTransitionPlan{plan.MachineUID: plan}}
	for _, local := range []struct {
		key       wgtypes.Key
		uid, peer string
	}{{root, "cloud-uid", site.PublicKey().String()}, {site, "site-uid", root.PublicKey().String()}} {
		peers, err := selectedReceiveLegacyPeers(local.key, local.uid, intent)
		if err != nil || len(peers) != 1 || peers[0].PublicKey != local.peer {
			t.Fatalf("selected legacy peer differs: %v", err)
		}
	}
	view.ServingGeneration = "receivers:other-generation"
	intent.Views[plan.MachineUID] = view
	if _, err := selectedReceiveLegacyPeers(root, "cloud-uid", intent); err == nil {
		t.Fatal("forward generation authorized legacy recovery")
	}
	view.ServingGeneration = "legacy:" + tunnel.HashPeerList(plan.Base)
	intent.Views[plan.MachineUID] = view
	delete(intent.Plans, plan.MachineUID)
	if _, err := selectedReceiveLegacyPeers(root, "cloud-uid", intent); err == nil {
		t.Fatal("unadmitted legacy plan was accepted")
	}
	intent.Legacy = map[string]tunnel.ReceiveTransitionPlan{}
	peers, err := selectedReceiveLegacyPeers(root, "cloud-uid", intent)
	if err != nil || len(peers) != 0 {
		t.Fatal("unselected legacy peers were returned", err)
	}
}

func TestNativeRestoreSelectedLegacyPeers(t *testing.T) {
	if os.Getenv("CLDT_RESTORE_LEGACY") != "1" {
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
	plan := receiveRestoreTestPlan(t, root, site)
	raw, err := json.Marshal(plan)
	must(err)
	spec, err := tunnel.ReceiveTransitionSpec(plan, "cluster", fmt.Sprintf("%x", sha256.Sum256(raw)))
	must(err)
	spec.Phase = handover.Abort
	view := handover.View{Round: handover.Round{Spec: spec}, Complete: true, RollbackRequested: true, ServingGeneration: spec.FromGeneration}
	intent := tunnel.ReceiveJournalRoutes{Views: map[string]handover.View{plan.MachineUID: view}, Plans: map[string]tunnel.ReceiveTransitionPlan{plan.MachineUID: plan}, Legacy: map[string]tunnel.ReceiveTransitionPlan{plan.MachineUID: plan}, Receivers: map[string]map[string]tunnel.ReceiveRouteSelection{"cloud-uid": {}, "site-uid": {}}}
	intent.Sources, err = tunnel.PlanReceiveSourceRoutes(intent)
	must(err)
	cloudCfg := config{iface: "legacycloud", nodeName: "cloud", mtu: 1400, fwmark: 518, routeTable: 518, listenPort: 51820, keepaliveSecs: 15}
	siteCfg := cloudCfg
	siteCfg.iface, siteCfg.nodeName, siteCfg.listenPort = "legacysite", "site", 0
	ctx := context.Background()
	restoreCloud := func() error {
		return restoreCompletedReceiveState(ctx, devices, cloudCfg, root, plan, view, "cloud-uid", intent)
	}
	restoreSite := func() error {
		return restoreCompletedReceiveState(ctx, devices, siteCfg, site, plan, view, "site-uid", intent)
	}
	if err := restoreCloud(); err == nil {
		t.Fatal("unconnected legacy peer passed verification")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		cloudErr, siteErr := restoreCloud(), restoreSite()
		if cloudErr == nil && siteErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cloud=%v site=%v", cloudErr, siteErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, name := range []string{plan.Bindings[site.PublicKey().String()].Interface, plan.Sites[site.PublicKey().String()].Client.Interface} {
		if _, err := netlink.LinkByName(name); !isLinkNotFound(err) {
			t.Fatal("legacy recovery recreated retired receiver", err)
		}
	}
	// An ordinary neighbor is outside this recovery partition.
	neighbor, err := wgtypes.GeneratePrivateKey()
	must(err)
	_, prefix, err := net.ParseCIDR("192.0.2.0/24")
	must(err)
	must(devices.ConfigureDevice(cloudCfg.iface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: neighbor.PublicKey(), AllowedIPs: []net.IPNet{*prefix}}}}))
	before, err := devices.Device(cloudCfg.iface)
	must(err)
	must(restoreCloud())
	after, err := devices.Device(cloudCfg.iface)
	must(err)
	if !samePeerConfiguration(before.Peers, after.Peers) {
		t.Fatal("recovery changed another peer")
	}
	// No recovery is allowed to move source ownership back from a live peer.
	_, stolen, err := net.ParseCIDR("10.90.0.0/24")
	must(err)
	must(devices.ConfigureDevice(cloudCfg.iface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: neighbor.PublicKey(), AllowedIPs: []net.IPNet{*stolen}}}}))
	before, err = devices.Device(cloudCfg.iface)
	must(err)
	if err := restoreCloud(); err == nil {
		t.Fatal("recovery stole another peer's source permissions")
	}
	after, err = devices.Device(cloudCfg.iface)
	must(err)
	if !samePeerConfiguration(before.Peers, after.Peers) {
		t.Fatal("rejected recovery changed peers")
	}
	must(devices.ConfigureDevice(cloudCfg.iface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: neighbor.PublicKey(), Remove: true}}}))
	must(restoreCloud())
	// Forward-selected intent cannot authorize replay of the legacy partition.
	forward := intent
	forward.Legacy = map[string]tunnel.ReceiveTransitionPlan{}
	before, err = devices.Device(cloudCfg.iface)
	must(err)
	must(restoreReceiveLegacyPeers(ctx, devices, cloudCfg, root, "cloud-uid", forward))
	after, err = devices.Device(cloudCfg.iface)
	must(err)
	if !samePeerConfiguration(before.Peers, after.Peers) {
		t.Fatal("unselected legacy partition changed")
	}
	// Reverse Retire selects the same legacy configuration after completion.
	view.Round.Phase = handover.Retire
	view.Round.FromGeneration, view.Round.ToGeneration = spec.ToGeneration, spec.FromGeneration
	view.Round.Members = append([]handover.Member(nil), spec.Members...)
	for i := range view.Round.Members {
		m := &view.Round.Members[i]
		m.FromKey, m.ToKey = m.ToKey, m.FromKey
		m.FromPeerKey, m.ToPeerKey = m.ToPeerKey, m.FromPeerKey
	}
	intent.Views[plan.MachineUID] = view
	must(restoreCloud())
	must(restoreSite())
}
