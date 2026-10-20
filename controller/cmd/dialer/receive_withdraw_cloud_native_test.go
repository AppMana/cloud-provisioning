package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

// Isolated guest mount/network namespaces only. Real kernel peer/route removal
// and executable cache updates; synthetic completed journals and Kubernetes API.
// No packet continuity, external routing, real token authentication or AWS
// lifecycle qualification. Each execution uses the executor's 30-second bound;
// stale adoption must produce no observation and leave native state installed.
func TestNativeCloudNeighborWithdrawal(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_CLOUD_WITHDRAWAL") != "1" {
		t.Skip("requires isolated guest namespaces")
	}
	for _, phase := range []string{"", "prepare", "switch", "drain", "retire", "abort"} {
		name := phase
		if name == "" {
			name = "completed"
		}
		t.Run(name, func(t *testing.T) { nativeCloudNeighborWithdrawal(t, phase) })
	}
}

func TestNativeJointCloudNeighborWithdrawal(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_CLOUD_WITHDRAWAL") != "1" {
		t.Skip("requires isolated guest namespaces")
	}
	for _, phase := range []string{"", "prepare", "switch", "drain", "retire", "abort"} {
		name := phase
		if name == "" {
			name = "completed"
		}
		t.Run(name, func(t *testing.T) { nativeCloudNeighborWithdrawal(t, phase, true) })
	}
}

func nativeCloudNeighborWithdrawal(t *testing.T, phase string, joint ...bool) {
	for _, kind := range []string{"net", "mnt"} {
		self, e := os.Readlink("/proc/self/ns/" + kind)
		host, h := os.Readlink("/proc/1/ns/" + kind)
		if e != nil || h != nil || self == host {
			t.Fatal("refusing guest host namespace")
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	site, _ := wgtypes.GeneratePrivateKey()
	targetKey, _ := wgtypes.GeneratePrivateKey()
	localKey, _ := wgtypes.GeneratePrivateKey()
	foreignKey, _ := wgtypes.GeneratePrivateKey()
	target := receiveRestoreTestPlan(t, targetKey, site)
	plans := []tunnel.ReceiveTransitionPlan{target}
	if len(joint) != 0 && joint[0] {
		second, _ := secondNativeWithdrawal(t, site)
		plans = append(plans, second)
	}
	local := receiveRestoreTestPlan(t, localKey, site)
	raw, err := json.Marshal(local)
	must(err)
	raw = []byte(strings.NewReplacer("172.29.0.1", "172.29.0.3", "fd29::1", "fd29::3").Replace(string(raw)))
	must(json.Unmarshal(raw, &local))
	local.MachineName, local.MachineUID = "survivor", "survivor-uid"
	local.CloudNodeName, local.CloudNodeUID = "survivor", "survivor-node-uid"
	local.AdoptionName, local.AdoptionUID = tunnel.AdoptionSecretName(local.MachineName), "survivor-adoption"
	cfg := config{iface: "withdrawcloud", nodeName: local.CloudNodeName, peersFile: filepath.Join(t.TempDir(), "peers.json"), peersSecretNamespace: "test", peersSecretName: local.AdoptionName, receiptMeshName: "mesh", receiptPodName: "agent", receiptPodUID: "agent-uid", routeTable: 518, fwmark: 518, mtu: 1400}
	oldPeers := append([]tunnel.PeerSpec{}, local.Desired.Peers...)
	for _, member := range plans {
		oldPeers = append(oldPeers, member.SiteDesired.Peers...)
	}
	bootstrap := tunnel.PeersFileDoc{PrivateKey: localKey.String(), LocalAddress: local.CloudAddresses[0], LocalAddress6: local.CloudAddresses[1], Peers: oldPeers, APIServers: []string{"10.90.0.2:6443"}}
	must(writeDurableJSON(cfg.peersFile, bootstrap))
	must(writeDurableJSON(cachePath(cfg), tunnel.PeerListDoc{Peers: oldPeers, APIServers: bootstrap.APIServers}))
	localJournal := cloudWithdrawalTestJournal(t, local, "local-journal")
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "mesh", UID: types.UID(target.MeshUID), ResourceVersion: "1"}, Data: map[string][]byte{
		tunnel.NodePublicKeyPrefix + "site": []byte(site.PublicKey().String()), tunnel.NodeTunnelAddressPrefix + "site": []byte("10.90.0.2/24"), tunnel.NodeTunnelAddress6Prefix + "site": []byte("fd90::2/64"), tunnel.NodeAddressesPrefix + "site": []byte("10.90.0.2,fd90::2"),
		tunnel.PeerPublicKeyPrefix + local.MachineName: []byte(local.RootPublicKey), tunnel.PeerEndpointPrefix + local.MachineName: []byte("198.51.100.3:51820"), tunnel.PeerAllowedIPsPrefix + local.MachineName: []byte("172.29.0.3/32,fd29::3/128"), tunnel.PeerRouteHostsPrefix + local.MachineName: []byte("172.29.0.3,fd29::3"),
	}}
	for _, member := range plans {
		addNativeWithdrawalPeer(mesh, member)
	}
	consumers := []attachment.ConsumerTarget{
		{Site: true, NodeName: "site", NodeUID: "site-uid", PublicKey: site.PublicKey().String()},
		{NodeName: local.CloudNodeName, NodeUID: local.CloudNodeUID, MachineName: local.MachineName, SecretUID: local.AdoptionUID, TunnelAddress: "172.29.0.3", PublicKey: local.RootPublicKey},
	}
	inventory, objects := nativeWithdrawalJournals(t, mesh, plans, consumers, phase)
	inventory.MachineUIDs = append(inventory.MachineUIDs, local.MachineUID)
	slices.Sort(inventory.MachineUIDs)
	inventory.Publications[local.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(localJournal.UID)}
	for _, member := range plans {
		mesh.Data, _, err = tunnel.WithdrawRemotePeer(mesh.Data, member.MachineName, member.RootPublicKey)
		must(err)
	}
	mesh.ResourceVersion = "2"
	inventoryRaw, err := json.Marshal(inventory)
	must(err)
	boot, err := localReceiveBootID()
	must(err)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: local.CloudNodeName, UID: types.UID(local.CloudNodeUID), ResourceVersion: "1"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: boot}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "agent", UID: "agent-uid"}, Spec: corev1.PodSpec{NodeName: local.CloudNodeName}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "dialer", Ready: true, ContainerID: "agent", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}}}}}
	published, err := json.Marshal(tunnel.PeerListDoc{Peers: oldPeers})
	must(err)
	adoption := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: local.AdoptionName, UID: types.UID(local.AdoptionUID), ResourceVersion: "1"}, Data: map[string][]byte{tunnel.CloudPeersKey: published}}
	objects = append(objects, mesh, localJournal, node, pod, adoption, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: tunnel.ReceiveInventoryName(target.MeshUID), UID: "inventory", ResourceVersion: "1"}, Data: map[string]string{"inventory.json": string(inventoryRaw)}})
	api := fake.NewClientset(objects...)
	devices, err := wgctrl.New()
	must(err)
	defer devices.Close()
	cfg.receiveRecoveryDevices = devices
	link := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: cfg.iface}}
	must(netlink.LinkAdd(link))
	defer netlink.LinkDel(link)
	must(netlink.LinkSetUp(link))
	must(ensureRouteRule(cfg.routeTable, cfg.fwmark))
	defer removeRouteRule(cfg.routeTable, cfg.fwmark)
	for _, raw := range local.CloudAddresses {
		a, e := netlink.ParseAddr(raw)
		must(e)
		must(netlink.AddrAdd(link, a))
	}
	allowed := func(raw string) net.IPNet { _, p, e := net.ParseCIDR(raw); must(e); return *p }
	// Preserve an unrelated root peer; the local worker serves the site through its receiver.
	peers := []wgtypes.PeerConfig{{PublicKey: foreignKey.PublicKey(), AllowedIPs: []net.IPNet{allowed("198.18.0.1/32")}}}
	retiredIPs := []string{}
	for _, member := range plans {
		public, err := wgtypes.ParseKey(member.RootPublicKey)
		must(err)
		ips := []net.IPNet{}
		for _, raw := range member.CloudAddresses {
			ips = append(ips, allowed(raw))
		}
		peers = append(peers, wgtypes.PeerConfig{PublicKey: public, AllowedIPs: ips})
		retiredIPs = append(retiredIPs, member.CloudAddresses...)
	}
	must(devices.ConfigureDevice(cfg.iface, wgtypes.Config{PrivateKey: &localKey, FirewallMark: &cfg.fwmark, Peers: peers}))
	selections, err := receivePhaseSelections(localKey, local, local.CloudNodeUID)
	must(err)
	selected := selections[0]
	receiver := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: selected.Interface, Alias: selected.Alias}}
	must(netlink.LinkAdd(receiver))
	defer func() {
		if current, err := netlink.LinkByName(selected.Interface); err == nil {
			_ = netlink.LinkDel(current)
		}
	}()
	must(netlink.LinkSetAlias(receiver, selected.Alias))
	must(netlink.LinkSetUp(receiver))
	scopeName, err := local.ReceiveScope()
	must(err)
	receiverPrivate := receiverKey(localKey, scopeName, site.PublicKey().String())
	receiverPort := local.Bindings[site.PublicKey().String()].ListenPort
	must(devices.ConfigureDevice(selected.Interface, wgtypes.Config{PrivateKey: &receiverPrivate, ListenPort: &receiverPort, FirewallMark: &cfg.fwmark, Peers: []wgtypes.PeerConfig{{PublicKey: site.PublicKey(), AllowedIPs: []net.IPNet{allowed("10.90.0.0/24"), allowed("fd90::/64")}}}}))
	receiverBefore, err := devices.Device(selected.Interface)
	must(err)
	for _, raw := range append(slices.Clone(retiredIPs), "10.90.0.0/24", "fd90::/64") {
		dst := allowed(raw)
		routeLink := link.Attrs().Index
		if raw == "10.90.0.0/24" || raw == "fd90::/64" {
			routeLink = receiver.Attrs().Index
		}
		must(netlink.RouteAdd(&netlink.Route{LinkIndex: routeLink, Dst: &dst, Table: cfg.routeTable, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT, Priority: receiveRoutePriority(&dst)}))
	}
	store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps("test"), ClusterUID: "native-cluster", Name: tunnel.ReceiveTransitionName(target.MachineUID)}
	scope := receiveJournalScope{Namespace: "test", MeshName: "mesh", MeshUID: target.MeshUID, MachineUID: target.MachineUID}
	run := func() ([]handover.PodReceipt, error) {
		return executeReceiveRetirement(ctx, api, store, scope, cfg, localKey, "agent", func(ctx context.Context, path string, a admittedReceiveRetirement) error {
			memberStore, memberScope := retirementMemberScope(store, scope, a, a.Withdrawal.MachineUID)
			return retireAdmittedCloudNeighborLocked(ctx, api, devices, memberStore, memberScope, cfg, localKey, path, a)
		})
	}
	receipts, failure := run()
	if failure == nil || !strings.Contains(failure.Error(), "remains in surviving cloud publication") || len(receipts) != 0 {
		t.Fatalf("stale adoption rejection: receipts=%d error=%v", len(receipts), failure)
	}
	live, err := devices.Device(cfg.iface)
	must(err)
	if len(live.Peers) != len(plans)+1 {
		t.Fatal("stale publication changed native peers")
	}
	if len(plans) > 1 {
		partial := local.Desired
		partial.Peers = append(slices.Clone(partial.Peers), plans[1].SiteDesired.Peers...)
		adoption.Data[tunnel.CloudPeersKey], err = json.Marshal(partial)
		must(err)
		adoption.ResourceVersion = "2"
		_, err = api.CoreV1().Secrets("test").Update(ctx, adoption, metav1.UpdateOptions{})
		must(err)
		receipts, failure = run()
		if failure == nil || !strings.Contains(failure.Error(), "remains in surviving cloud publication") || len(receipts) != 0 {
			t.Fatalf("second stale adoption emitted joint receipt: %v", failure)
		}
		live, err = devices.Device(cfg.iface)
		must(err)
		if len(live.Peers) != 2 {
			t.Fatal("partial cleanup lost surviving peers", failure)
		}
		for _, peer := range live.Peers {
			if peer.PublicKey == targetKey.PublicKey() {
				t.Fatal("first cleanup did not precede second rejection", failure)
			}
		}
	}
	adoption.Data[tunnel.CloudPeersKey], err = json.Marshal(local.Desired)
	must(err)
	adoption.ResourceVersion = "3"
	_, err = api.CoreV1().Secrets("test").Update(ctx, adoption, metav1.UpdateOptions{})
	must(err)
	for i := 0; i < 2; i++ {
		receipts, err := run()
		must(err)
		if len(receipts) != 1 {
			t.Fatal("missing cleanup observation")
		}
	}
	live, err = devices.Device(cfg.iface)
	must(err)
	if live.PublicKey != localKey.PublicKey() || len(live.Peers) != 1 || live.Peers[0].PublicKey != foreignKey.PublicKey() {
		t.Fatal("cleanup changed identity or surviving peer")
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
	must(err)
	if len(routes) != 2 {
		t.Fatal("unexpected remaining routes", routes)
	}
	for _, route := range routes {
		if route.Dst == nil || (route.Dst.String() != "10.90.0.0/24" && route.Dst.String() != "fd90::/64") || route.LinkIndex != receiver.Attrs().Index || route.Protocol != unix.RTPROT_BOOT || route.Priority != receiveRoutePriority(route.Dst) {
			t.Fatal("surviving route changed", route)
		}
	}
	receiverAfter, err := devices.Device(selected.Interface)
	must(err)
	if !reflect.DeepEqual(receiverBefore, receiverAfter) {
		t.Fatal("surviving receiver changed")
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	must(err)
	for _, raw := range local.CloudAddresses {
		found := false
		for _, address := range addresses {
			found = found || address.IPNet.String() == raw
		}
		if !found {
			t.Fatal("stable allocation removed", raw)
		}
	}
	must(verifyReceivePolicyRules(cfg))
	saved, err := readPeersFileDoc(cfg.peersFile)
	must(err)
	if saved.PrivateKey != bootstrap.PrivateKey || !reflect.DeepEqual(fileTunnelAddresses(saved), local.CloudAddresses) || !reflect.DeepEqual(saved.APIServers, bootstrap.APIServers) || !reflect.DeepEqual(saved.Peers, local.Desired.Peers) {
		t.Fatal("bootstrap cleanup changed survivor")
	}
	cached, err := os.ReadFile(cachePath(cfg))
	must(err)
	for _, member := range plans {
		if strings.Contains(string(cached), member.RootPublicKey) {
			t.Fatal("executable cache retained target")
		}
	}
	// Recover a lost survivor route after complete withdrawal. Authenticate the
	// existing receiver against an isolated loopback endpoint before admitting
	// native recovery; absence of the retired peer never authorizes its return.
	lo, err := netlink.LinkByName("lo")
	must(err)
	must(netlink.LinkSetUp(lo))
	endpoint, err := netlink.ParseAddr("198.51.100.1/32")
	must(err)
	must(netlink.AddrAdd(lo, endpoint))
	defer netlink.AddrDel(lo, endpoint)
	_, err = prepareSiteReceiveClient(devices, local.MeshUID, site, local.RootPublicKey, local.Bindings[site.PublicKey().String()], local.SiteDesired, local.SiteDesired, cfg.mtu, cfg.fwmark)
	must(err)
	defer func() {
		if client, err := netlink.LinkByName(local.Sites[site.PublicKey().String()].Client.Interface); err == nil {
			_ = netlink.LinkDel(client)
		}
	}()
	survivorStore, survivorScope := store, scope
	survivorStore.Name, survivorScope.MachineUID = localJournal.Name, local.MachineUID
	for _, lost := range routes {
		must(netlink.RouteDel(&lost))
		deadline := time.Now().Add(10 * time.Second)
		for {
			err = maintainCompletedReceivePhase(ctx, api, survivorStore, survivorScope, cfg, localKey, "agent", func(ctx context.Context, p tunnel.ReceiveTransitionPlan, v handover.View, uid string, serving tunnel.ReceiveJournalRoutes) error {
				if len(serving.Plans) != 1 {
					return fmt.Errorf("survivor membership changed")
				}
				for _, member := range plans {
					if serving.Withdrawn[member.MachineUID] != member.RootPublicKey {
						return fmt.Errorf("survivor lost withdrawn ownership")
					}
				}
				if err := restoreCompletedReceiveState(ctx, devices, cfg, localKey, p, v, uid, serving); err != nil {
					return err
				}
				return receiveLinuxPhase(ctx, devices, cfg, localKey, nil, local.CloudAddresses, p, v, uid, serving)
			})
			if err == nil {
				if time.Now().After(deadline) {
					t.Fatal("survivor route recovery exceeded ten seconds")
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("survivor maintenance after withdrawal", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	routes, err = netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
	must(err)
	if len(routes) != 2 {
		t.Fatal("maintenance failed to restore exactly the survivor routes", routes)
	}
	for _, route := range routes {
		if route.Dst == nil || (route.Dst.String() != "10.90.0.0/24" && route.Dst.String() != "fd90::/64") || route.LinkIndex != receiver.Attrs().Index {
			t.Fatal("maintenance restored an unrelated destination", route)
		}
	}
	live, err = devices.Device(cfg.iface)
	must(err)
	if len(live.Peers) != 1 || live.Peers[0].PublicKey != foreignKey.PublicKey() {
		t.Fatal("survivor maintenance changed unrelated root peer")
	}
	for _, peer := range live.Peers {
		if peer.PublicKey == targetKey.PublicKey() {
			t.Fatal("maintenance revived withdrawn root peer")
		}
	}
	// Rebind actual successful readback to a simulated previous boot, then lose
	// the receiver. Keep all emitted files so offline admission sees the same
	// retained inventory and auxiliary checkpoints that a restart would see.
	lockPath := filepath.Join(filepath.Dir(cfg.peersFile), cfg.iface+".reconcile.lock")
	inventoryPath := lockPath + ".inventory.json"
	journalPath := lockPath + "." + localJournal.Name + ".json"
	observationPath := receiveNativeObservationPath(journalPath, localJournal.Name)
	read := func(path string, value any) {
		t.Helper()
		raw, err := os.ReadFile(path)
		must(err)
		must(json.Unmarshal(raw, value))
	}
	var bootInventory receiveInventoryCheckpoint
	var bootJournal receiveJournalCheckpoint
	var bootObservation receiveNativeObservation
	read(inventoryPath, &bootInventory)
	read(journalPath, &bootJournal)
	read(observationPath, &bootObservation)
	var bootEndpoints receiveBootEndpoints
	read(receiveBootEndpointsPath(observationPath), &bootEndpoints)
	bootInventory.BootID, bootJournal.BootID, bootObservation.BootID = "previous-boot", "previous-boot", "previous-boot"
	must(writeDurableJSON(inventoryPath, bootInventory))
	must(writeDurableJSON(journalPath, bootJournal))
	inventoryBytes, err := os.ReadFile(inventoryPath)
	must(err)
	bootObservation.InventorySHA256 = fmt.Sprintf("%x", sha256.Sum256(inventoryBytes))
	must(writeDurableJSON(observationPath, bootObservation))
	must(writeReceiveBootEndpoints(observationPath, bootEndpoints.Endpoints))
	must(netlink.LinkDel(receiver))
	deadline := time.Now().Add(10 * time.Second)
	for {
		handled, err := recoverReceiveBoot(ctx, devices, cfg)
		if !handled {
			t.Fatal("survivor reboot fell through to legacy")
		}
		if err == nil {
			if time.Now().After(deadline) {
				t.Fatal("survivor boot recovery exceeded ten seconds")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("survivor boot recovery", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	routes, err = netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
	must(err)
	if len(routes) != 2 {
		t.Fatal("boot recovery lost survivor routes", routes)
	}
	for _, route := range routes {
		if route.Dst == nil || (route.Dst.String() != "10.90.0.0/24" && route.Dst.String() != "fd90::/64") {
			t.Fatal("boot recovery revived withdrawn destination", route)
		}
	}
	live, err = devices.Device(cfg.iface)
	must(err)
	if len(live.Peers) != 1 || live.Peers[0].PublicKey != foreignKey.PublicKey() {
		t.Fatal("boot recovery changed unrelated peers or revived withdrawal")
	}
}

func cloudWithdrawalTestJournal(t *testing.T, plan tunnel.ReceiveTransitionPlan, uid string, interrupted ...string) *corev1.ConfigMap {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(plan)
	must(err)
	spec, err := tunnel.ReceiveTransitionSpec(plan, "native-cluster", fmt.Sprintf("%x", sha256.Sum256(raw)))
	must(err)
	at := time.Now().Add(-10 * time.Second)
	phase := ""
	if len(interrupted) > 0 {
		phase = interrupted[0]
	}
	if phase != "" {
		at = time.Now().Add(-time.Hour)
	}
	transition, err := handover.NewTransition(spec, at, handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: time.Minute, DrainInterval: time.Second})
	must(err)
	if phase == "abort" {
		transition, err = transition.Rollback(at.Add(time.Second))
		must(err)
	}
	for i := 0; i < 6; i++ {
		view, err := transition.View()
		must(err)
		if view.Complete || string(view.Round.Phase) == phase {
			break
		}
		at = at.Add(time.Second)
		if at.Before(view.DrainUntil) {
			at = view.DrainUntil
		}
		digest, err := view.Round.Digest()
		must(err)
		proofs := []handover.AuthenticatedReceipt{}
		for _, m := range view.Round.Members {
			proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: digest, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: at}})
		}
		transition, err = transition.Advance(at, proofs)
		must(err)
	}
	history, err := transition.Bytes()
	must(err)
	immutable := true
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: plan.Namespace, Name: tunnel.ReceiveTransitionName(plan.MachineUID), UID: types.UID(uid), ResourceVersion: "1"}, Immutable: &immutable, Data: map[string]string{"transition.json": string(history), "plan.json": string(raw)}}
}
