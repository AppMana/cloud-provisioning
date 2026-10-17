package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
		Plan              tunnel.ReceiveTransitionPlan `json:"plan"`
		KeyFile           string                       `json:"keyFile"`
		Action            string                       `json:"action"`
		NodeName          string                       `json:"nodeName"`
		Setup             bool                         `json:"setup"`
		StateFile         string                       `json:"stateFile"`
		ExpectWait        bool                         `json:"expectWait"`
		CloudAddresses    []string                     `json:"cloudAddresses"`
		ContinuityOnly    bool                         `json:"continuityOnly"`
		CompletedReadback bool                         `json:"completedReadback"`
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
	reverse := request.Action == "rollback" || request.Action == "rollback-drain" || request.Action == "rollback-retire"
	switch request.Action {
	case "prepare":
	case "abort":
		tr, err = tr.Rollback(start.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
	case "switch", "drain", "retire", "rollback", "rollback-drain", "rollback-retire":
		advance(start.Add(time.Second))
		if reverse {
			tr, err = tr.Rollback(start.Add(2 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
		}
		if request.Action == "drain" || request.Action == "retire" || request.Action == "rollback-drain" || request.Action == "rollback-retire" {
			advance(start.Add(3 * time.Second))
		}
		if request.Action == "retire" || request.Action == "rollback-retire" {
			advance(start.Add(4 * time.Second))
		}
	default:
		t.Fatal("unsupported test action")
	}
	if request.StateFile != "" {
		stored, err := os.ReadFile(request.StateFile)
		if err == nil {
			tr, err = handover.DecodeTransition(stored)
		} else if os.IsNotExist(err) {
			stored, err = tr.Bytes()
			if err == nil {
				err = os.WriteFile(request.StateFile, stored, 0600)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if request.CompletedReadback {
		terminal, err := tr.View()
		if err != nil || (terminal.Round.Phase != handover.Retire && terminal.Round.Phase != handover.Abort) {
			t.Fatal("terminal fixture requires retirement or abort", err)
		}
		advance(time.Now())
	}
	view, err := tr.View()
	if err != nil {
		t.Fatal(err)
	}
	receivers, err := tunnel.PlanReceiveRoutes(plan)
	if err != nil {
		t.Fatal(err)
	}
	intent := tunnel.ReceiveJournalRoutes{Views: map[string]handover.View{plan.MachineUID: view}, Plans: map[string]tunnel.ReceiveTransitionPlan{plan.MachineUID: plan}, Legacy: map[string]tunnel.ReceiveTransitionPlan{}, Receivers: receivers}
	if reverse || request.Action == "prepare" || request.Action == "abort" {
		intent.Legacy[plan.MachineUID] = plan
		intent.Receivers = map[string]map[string]tunnel.ReceiveRouteSelection{}
		for node := range receivers {
			intent.Receivers[node] = map[string]tunnel.ReceiveRouteSelection{}
		}
	}
	for _, site := range plan.Sites {
		if site.Legacy != nil && site.Legacy.Tunnel != nil {
			intent.Sources, err = tunnel.PlanReceiveSourceRoutes(intent)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	cfg := config{iface: "wglegacy", nodeName: plan.CloudNodeName, routeTable: 518, fwmark: 518, mtu: 1400}
	cloudAddresses := request.CloudAddresses
	if len(cloudAddresses) == 0 {
		cloudAddresses = []string{"10.88.0.3/32"}
	}
	publish := func(nodeUID string) {
		if request.StateFile == "" || view.Complete {
			return
		}
		digest, err := view.Round.Digest()
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		var proofs []handover.AuthenticatedReceipt
		for _, m := range view.Round.Members {
			if m.NodeUID != nodeUID {
				continue
			}
			proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: nodeUID, Receipt: handover.Receipt{NodeUID: nodeUID, DeviceID: m.DeviceID, RoundHash: digest, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: now}})
		}
		next, err := tr.Observe(now, proofs)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := next.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(request.StateFile, stored, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Continuity runs exclude the deliberate foreign-route and missing-state
	// injections below; those assertions remain enabled in the default mode.
	if request.NodeName != "" || request.Setup || request.ContinuityOnly {
		nodeUID, addresses := plan.CloudNodeUID, cloudAddresses
		if request.NodeName != "" {
			site, found := plan.Sites[key.PublicKey().String()]
			if !found || site.NodeName != request.NodeName || site.Legacy == nil || site.Legacy.Tunnel == nil {
				t.Fatal("site fixture lacks committed identity or allocation")
			}
			cfg.nodeName, cfg.iface = site.NodeName, "wglegacytest"
			nodeUID, addresses = site.NodeUID, site.Legacy.Tunnel.Addresses
		}
		if request.Setup {
			// Establish the experiment's initial applied generation, using the
			// same route operations as normal reconciliation. Phase execution
			// below never repairs a fixture or substitutes for product behavior.
			var peers []tunnel.PeerSpec
			var sourceHosts []net.IPNet
			cfg.receiveRouteScope = map[string]bool{}
			if request.NodeName == "" {
				var base tunnel.PeerListDoc
				if err := json.Unmarshal(plan.Base, &base); err != nil {
					t.Fatal(err)
				}
				for _, peer := range base.Peers {
					if !peer.Remote {
						peers = append(peers, peer)
					}
				}
			} else {
				peer, err := receiveSiteLegacyPeer(plan, key.PublicKey().String())
				if err != nil {
					t.Fatal(err)
				}
				peers = append(peers, peer)
				site := plan.Sites[key.PublicKey().String()]
				cfg.receiveRelayRoutes = map[string]string{}
				if site.Legacy.Transit != nil {
					for raw := range receivers[nodeUID] {
						_, dst, _ := net.ParseCIDR(raw)
						cfg.receiveRelayRoutes[raw] = site.Legacy.Transit.Gateway(dst.IP).String()
					}
				}
				for _, raw := range site.Legacy.Tunnel.Hosts {
					_, dst, _ := net.ParseCIDR(raw)
					sourceHosts = append(sourceHosts, *dst)
				}
			}
			for _, peer := range peers {
				for _, raw := range peer.WGAllowedIPs {
					cfg.receiveRouteScope[raw] = true
				}
				for _, raw := range peer.AllRouteHosts() {
					cfg.receiveRouteScope[tunnel.HostCIDR(raw)] = true
				}
			}
			var hosts []net.IPNet
			for raw := range cfg.receiveRouteScope {
				_, dst, _ := net.ParseCIDR(raw)
				hosts = append(hosts, *dst)
			}
			if err := installRoutes(cfg, hosts, nil, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := reconcileTunnelSourceRoutes(cfg, addresses, sourceHosts); err != nil {
				t.Fatal(err)
			}
			return
		}
		if request.ExpectWait {
			before, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
			if err != nil {
				t.Fatal(err)
			}
			err = receiveLinuxPhase(context.Background(), devices, cfg, key, nil, addresses, plan, view, nodeUID, intent)
			if err == nil || !strings.Contains(err.Error(), "relay forwarding dependency") {
				t.Fatal("unsafe switch did not wait for its relay", err)
			}
			after, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("waiting switch changed workload routes", err)
			}
			return
		}
		var verifier receiverDevices = devices
		var beforeRoutes []netlink.Route
		var beforeAddresses []netlink.Addr
		if request.CompletedReadback {
			verifier = readOnlyReceiveDevices{devices}
			beforeRoutes, err = netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
			if err != nil {
				t.Fatal(err)
			}
			beforeAddresses, err = netlink.AddrList(nil, netlink.FAMILY_ALL)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := receiveLinuxPhase(context.Background(), verifier, cfg, key, nil, addresses, plan, view, nodeUID, intent); err != nil {
			t.Fatal(err)
		}
		if request.CompletedReadback {
			afterRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
			if err != nil || !reflect.DeepEqual(beforeRoutes, afterRoutes) {
				t.Fatal("terminal readback mutated routes", err)
			}
			afterAddresses, err := netlink.AddrList(nil, netlink.FAMILY_ALL)
			if err != nil || !reflect.DeepEqual(beforeAddresses, afterAddresses) {
				t.Fatal("terminal readback mutated addresses", err)
			}
		}
		publish(nodeUID)
		return
	}
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
	hasUnrelated := false
	for _, peer := range before.Peers {
		for _, prefix := range peer.AllowedIPs {
			hasUnrelated = hasUnrelated || prefix.String() == addr.IPNet.String()
		}
	}
	if !hasUnrelated {
		extra, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		if err := devices.ConfigureDevice(cfg.iface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: extra.PublicKey(), AllowedIPs: []net.IPNet{*addr.IPNet}}}}); err != nil {
			t.Fatal(err)
		}
		before, err = devices.Device(cfg.iface)
		if err != nil {
			t.Fatal(err)
		}
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
			err := receiveLinuxPhase(context.Background(), devices, cfg, key, nil, cloudAddresses, plan, view, plan.CloudNodeUID, intent)
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
		if err := receiveLinuxPhase(context.Background(), devices, cfg, key, nil, cloudAddresses, plan, view, plan.CloudNodeUID, intent); err == nil {
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
	var phaseDevices receiverDevices = devices
	retiring := view.Round.Phase == handover.Retire || view.Round.Phase == handover.Abort
	if retiring {
		phaseDevices = retirementOnlyDevices{receiverDevices: devices}
		selections, err := receivePhaseSelections(key, plan, plan.CloudNodeUID)
		if err != nil {
			t.Fatal(err)
		}
		indexes := map[string]int{}
		states := map[string]*wgtypes.Device{}
		for _, selection := range selections {
			link, err := netlink.LinkByName(selection.Interface)
			if err != nil {
				t.Fatal(err)
			}
			indexes[selection.Interface] = link.Attrs().Index
			states[selection.Interface], err = devices.Device(selection.Interface)
			if err != nil {
				t.Fatal(err)
			}
		}
		assertUnchanged := func() {
			t.Helper()
			current, err := devices.Device(cfg.iface)
			if err != nil || current.PrivateKey != before.PrivateKey || !samePeerConfiguration(before.Peers, current.Peers) {
				t.Fatal("rejected retirement changed legacy peers", err)
			}
			for name, saved := range states {
				link, err := netlink.LinkByName(name)
				if err != nil || link.Attrs().Index != indexes[name] {
					t.Fatal("rejected retirement changed receiver device", err)
				}
				current, err := devices.Device(name)
				if err != nil || current.PrivateKey != saved.PrivateKey || !samePeerConfiguration(saved.Peers, current.Peers) {
					t.Fatal("rejected retirement changed receiver peers", err)
				}
			}
		}
		allocation := stableAddressOwnerRequest{MeshUID: plan.MeshUID, NodeUID: plan.CloudNodeUID, LegacyInterface: cfg.iface, RootPublicKey: key.PublicKey().String(), Addresses: cloudAddresses}
		binding, err := verifyStableAddressOwner(allocation)
		if err != nil {
			t.Fatal(err)
		}
		owner, err := netlink.LinkByName(binding.Interface)
		if err != nil {
			t.Fatal(err)
		}
		address, err := netlink.ParseAddr(binding.Addresses[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.AddrDel(owner, address); err != nil {
			t.Fatal(err)
		}
		if err := receiveLinuxPhase(context.Background(), phaseDevices, cfg, key, nil, allocation.Addresses, plan, view, plan.CloudNodeUID, intent); err == nil {
			t.Fatal("retirement accepted a missing stable address")
		}
		if _, err := verifyStableAddressOwner(allocation); err == nil {
			t.Fatal("retirement repaired the stable address")
		}
		assertUnchanged()
		if err := netlink.AddrAdd(owner, address); err != nil {
			t.Fatal(err)
		}
		dependentLink := legacy
		if reverse || request.Action == "abort" {
			selections, err := receivePhaseSelections(key, plan, plan.CloudNodeUID)
			if err != nil {
				t.Fatal(err)
			}
			dependentLink, err = netlink.LinkByName(selections[0].Interface)
			if err != nil {
				t.Fatal(err)
			}
		}
		destination, err := netlink.ParseAddr("10.90.0.10/32")
		if err != nil {
			t.Fatal(err)
		}
		dependency := netlink.Route{LinkIndex: dependentLink.Attrs().Index, Dst: destination.IPNet, Table: 333, Scope: netlink.SCOPE_LINK}
		if err := netlink.RouteAdd(&dependency); err != nil {
			t.Fatal(err)
		}
		if err := receiveLinuxPhase(context.Background(), phaseDevices, cfg, key, nil, allocation.Addresses, plan, view, plan.CloudNodeUID, intent); err == nil {
			t.Fatal("retirement ignored another table's route dependency")
		}
		assertUnchanged()
		if err := netlink.RouteDel(&dependency); err != nil {
			t.Fatal("retirement removed the blocking route", err)
		}
	}
	for attempt := 0; ; attempt++ {
		err := receiveLinuxPhase(context.Background(), phaseDevices, cfg, key, nil, cloudAddresses, plan, view, plan.CloudNodeUID, intent)
		if err == nil {
			break
		}
		if view.Round.Phase != handover.Prepare || attempt >= 50 || !strings.Contains(err.Error(), "no current authenticated transport") {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if retiring {
		if err := receiveLinuxPhase(context.Background(), phaseDevices, cfg, key, nil, cloudAddresses, plan, view, plan.CloudNodeUID, intent); err != nil {
			t.Fatal("retirement retry", err)
		}
	}
	after, err := devices.Device(cfg.iface)
	if err != nil {
		t.Fatal(err)
	}
	expectedPeers := before.Peers
	if view.Round.Phase == handover.Retire && !reverse {
		var base tunnel.PeerListDoc
		if err := json.Unmarshal(plan.Base, &base); err != nil {
			t.Fatal(err)
		}
		expectedPeers = nil
		for _, peer := range before.Peers {
			retired := false
			for _, old := range base.Peers {
				retired = retired || (!old.Remote && old.PublicKey == peer.PublicKey.String())
			}
			if !retired {
				expectedPeers = append(expectedPeers, peer)
			}
		}
	}
	if before.PrivateKey != after.PrivateKey || !samePeerConfiguration(expectedPeers, after.Peers) {
		t.Fatal("native phase changed unrelated legacy receive permissions")
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
		if err := receiveLinuxPhase(context.Background(), devices, cfg, key, nil, cloudAddresses, plan, view, plan.CloudNodeUID, intent); err == nil {
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
	publish(plan.CloudNodeUID)
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

// Retire/abort retries are forbidden from recreating keys or peers.
type retirementOnlyDevices struct{ receiverDevices }

type readOnlyReceiveDevices struct{ receiverDevices }

func (d readOnlyReceiveDevices) ConfigureDevice(string, wgtypes.Config) error {
	return fmt.Errorf("terminal readback attempted a WireGuard mutation")
}

func (d retirementOnlyDevices) ConfigureDevice(name string, cfg wgtypes.Config) error {
	if cfg.PrivateKey != nil || cfg.ListenPort != nil || cfg.FirewallMark != nil || cfg.ReplacePeers {
		return fmt.Errorf("retirement attempted device reconfiguration")
	}
	for _, peer := range cfg.Peers {
		if !peer.Remove {
			return fmt.Errorf("retirement attempted peer preparation")
		}
	}
	return d.receiverDevices.ConfigureDevice(name, cfg)
}

func TestDirectSiteLegacyPeerRejectsUnsupportedSourceTransfers(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		before, after                []string
		remote, omitCloud, wantError bool
	}{
		{name: "unchanged dual stack", before: []string{"10.90.0.0/24", "2001:db8:1::/64"}, after: []string{"10.90.0.0/24", "2001:db8:1::/64"}},
		{name: "narrower desired sources", before: []string{"10.90.0.0/24"}, after: []string{"10.90.0.128/25"}},
		{name: "retained old sender", before: []string{"10.90.0.0/24"}},
		{name: "new site", after: []string{"10.90.0.0/24"}, wantError: true},
		{name: "bare retained peer gains workload", before: []string{"10.88.0.2/32"}, after: []string{"10.88.0.2/32", "10.90.0.0/24"}, wantError: true},
		{name: "larger source block", before: []string{"10.90.0.0/25"}, after: []string{"10.90.0.0/24"}, wantError: true},
		{name: "new IPv6 source", before: []string{"10.90.0.0/24"}, after: []string{"10.90.0.0/24", "2001:db8:1::/64"}, wantError: true},
		{name: "cloud role is not site", before: []string{"10.90.0.0/24"}, remote: true, wantError: true},
		{name: "missing reverse peer", before: []string{"10.90.0.0/24"}, omitCloud: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: "site", WGAllowedIPs: tc.before, Remote: tc.remote}, {PublicKey: "relay", WGAllowedIPs: []string{"10.91.0.0/24"}}}}
			raw, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			cloud := tunnel.PeerSpec{PublicKey: "cloud", Remote: true, WGAllowedIPs: []string{"172.29.0.2/32"}, Endpoint: "198.51.100.1:51820"}
			plan := tunnel.ReceiveTransitionPlan{Base: raw, RootPublicKey: "cloud", Desired: tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: "site", WGAllowedIPs: tc.after}}}, SiteDesired: tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{cloud}}}
			if tc.omitCloud {
				plan.SiteDesired.Peers = nil
			}
			before, _ := json.Marshal(plan)
			peer, err := receiveDirectSiteLegacyPeer(plan, "site")
			if (err != nil) != tc.wantError {
				t.Fatalf("peer=%+v err=%v", peer, err)
			}
			if err == nil && !reflect.DeepEqual(peer, cloud) {
				t.Fatal("changed legacy cloud peer")
			}
			after, _ := json.Marshal(plan)
			if string(before) != string(after) {
				t.Fatal("modified committed plan")
			}
		})
	}
}

func TestDirectSiteLegacyPeerUsesAdmittedWorkloadSources(t *testing.T) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	public := key.PublicKey().String()
	cloudKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	cloud := tunnel.PeerSpec{PublicKey: cloudKey.PublicKey().String(), WGAllowedIPs: []string{"172.29.0.1/32"}}
	base, _ := json.Marshal(tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: public, WGAllowedIPs: []string{"10.10.0.1/32"}}}})
	path := &tunnel.ReceiveSiteLegacy{Sources: []string{"10.10.0.1/32"}, PeerPublicKey: public}
	p := tunnel.ReceiveTransitionPlan{Base: base, RootPublicKey: cloud.PublicKey, SiteDesired: tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{cloud}}, Desired: tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: public, WGAllowedIPs: []string{"10.10.0.1/32", "10.90.0.0/24"}}}}, Sites: map[string]tunnel.ReceiveTransitionSite{public: {Legacy: path}}}
	if peer, err := receiveDirectSiteLegacyPeer(p, public); err != nil || !reflect.DeepEqual(peer, cloud) {
		t.Fatalf("admitted direct path: %+v %v", peer, err)
	}
	path.Sources = append(path.Sources, "10.90.0.0/24")
	if _, err := receiveDirectSiteLegacyPeer(p, public); err == nil {
		t.Fatal("accepted source not owned in legacy generation")
	}
}
