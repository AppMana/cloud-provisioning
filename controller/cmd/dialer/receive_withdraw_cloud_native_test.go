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
	oldPeers = append(oldPeers, target.SiteDesired.Peers...)
	bootstrap := tunnel.PeersFileDoc{PrivateKey: localKey.String(), LocalAddress: local.CloudAddresses[0], LocalAddress6: local.CloudAddresses[1], Peers: oldPeers, APIServers: []string{"10.90.0.2:6443"}}
	must(writeDurableJSON(cfg.peersFile, bootstrap))
	must(writeDurableJSON(cachePath(cfg), tunnel.PeerListDoc{Peers: oldPeers, APIServers: bootstrap.APIServers}))
	targetJournal := cloudWithdrawalTestJournal(t, target, "target-journal")
	localJournal := cloudWithdrawalTestJournal(t, local, "local-journal")
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "mesh", UID: types.UID(target.MeshUID), ResourceVersion: "2"}, Data: map[string][]byte{
		tunnel.NodePublicKeyPrefix + "site": []byte(site.PublicKey().String()), tunnel.NodeTunnelAddressPrefix + "site": []byte("10.90.0.2/24"), tunnel.NodeTunnelAddress6Prefix + "site": []byte("fd90::2/64"), tunnel.NodeAddressesPrefix + "site": []byte("10.90.0.2,fd90::2"),
		tunnel.PeerPublicKeyPrefix + local.MachineName: []byte(local.RootPublicKey), tunnel.PeerEndpointPrefix + local.MachineName: []byte("198.51.100.3:51820"), tunnel.PeerAllowedIPsPrefix + local.MachineName: []byte("172.29.0.3/32,fd29::3/128"), tunnel.PeerRouteHostsPrefix + local.MachineName: []byte("172.29.0.3,fd29::3"),
	}}
	intent := attachment.RemoteWithdrawalIntent{MeshNamespace: "test", MeshName: "mesh", MeshUID: target.MeshUID, SourceVersion: "1", MachineName: target.MachineName, MachineUID: target.MachineUID, PublicKey: target.RootPublicKey, DrainIntent: "native-test", Consumers: []attachment.ConsumerTarget{
		{Site: true, NodeName: "site", NodeUID: "site-uid", PublicKey: site.PublicKey().String()},
		{NodeName: local.CloudNodeName, NodeUID: local.CloudNodeUID, MachineName: local.MachineName, SecretUID: local.AdoptionUID, TunnelAddress: "172.29.0.3", PublicKey: local.RootPublicKey},
	}}
	retirement, err := json.Marshal(map[string]any{"version": 1, "journalSHA256": tunnel.HashPeerList([]byte(targetJournal.Data["transition.json"])), "withdrawal": intent})
	must(err)
	inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: "native-cluster", Namespace: "test", MeshName: "mesh", MeshUID: target.MeshUID, MachineUIDs: []string{local.MachineUID, target.MachineUID}, Publications: map[string]tunnel.ReceiveJournalPublication{
		target.MachineUID: {JournalUID: string(targetJournal.UID), Retirement: retirement}, local.MachineUID: {JournalUID: string(localJournal.UID)},
	}}
	inventoryRaw, err := json.Marshal(inventory)
	must(err)
	boot, err := localReceiveBootID()
	must(err)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: local.CloudNodeName, UID: types.UID(local.CloudNodeUID), ResourceVersion: "1"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: boot}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "agent", UID: "agent-uid"}, Spec: corev1.PodSpec{NodeName: local.CloudNodeName}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "dialer", Ready: true, ContainerID: "agent", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}}}}}
	published, err := json.Marshal(tunnel.PeerListDoc{Peers: oldPeers})
	must(err)
	adoption := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: local.AdoptionName, UID: types.UID(local.AdoptionUID), ResourceVersion: "1"}, Data: map[string][]byte{tunnel.CloudPeersKey: published}}
	api := fake.NewClientset(mesh, targetJournal, localJournal, node, pod, adoption, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: tunnel.ReceiveInventoryName(target.MeshUID), UID: "inventory", ResourceVersion: "1"}, Data: map[string]string{"inventory.json": string(inventoryRaw)}})
	devices, err := wgctrl.New()
	must(err)
	defer devices.Close()
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
	must(devices.ConfigureDevice(cfg.iface, wgtypes.Config{PrivateKey: &localKey, FirewallMark: &cfg.fwmark, Peers: []wgtypes.PeerConfig{
		{PublicKey: targetKey.PublicKey(), AllowedIPs: []net.IPNet{allowed("172.29.0.1/32"), allowed("fd29::1/128")}},
		// The completed survivor serves the site through its receiver. Preserve
		// an unrelated root peer, rather than inventing an unretired site peer.
		{PublicKey: foreignKey.PublicKey(), AllowedIPs: []net.IPNet{allowed("198.18.0.1/32")}},
	}}))
	selections, err := receivePhaseSelections(localKey, local, local.CloudNodeUID)
	must(err)
	selected := selections[0]
	receiver := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: selected.Interface, Alias: selected.Alias}}
	must(netlink.LinkAdd(receiver))
	defer netlink.LinkDel(receiver)
	must(netlink.LinkSetAlias(receiver, selected.Alias))
	must(netlink.LinkSetUp(receiver))
	scopeName, err := local.ReceiveScope()
	must(err)
	receiverPrivate := receiverKey(localKey, scopeName, site.PublicKey().String())
	receiverPort := local.Bindings[site.PublicKey().String()].ListenPort
	must(devices.ConfigureDevice(selected.Interface, wgtypes.Config{PrivateKey: &receiverPrivate, ListenPort: &receiverPort, FirewallMark: &cfg.fwmark, Peers: []wgtypes.PeerConfig{{PublicKey: site.PublicKey(), AllowedIPs: []net.IPNet{allowed("10.90.0.0/24"), allowed("fd90::/64")}}}}))
	receiverBefore, err := devices.Device(selected.Interface)
	must(err)
	for _, raw := range []string{"172.29.0.1/32", "fd29::1/128", "10.90.0.0/24", "fd90::/64"} {
		dst := allowed(raw)
		routeLink := link.Attrs().Index
		if raw == "10.90.0.0/24" || raw == "fd90::/64" {
			routeLink = receiver.Attrs().Index
		}
		must(netlink.RouteAdd(&netlink.Route{LinkIndex: routeLink, Dst: &dst, Table: cfg.routeTable, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT, Priority: receiveRoutePriority(&dst)}))
	}
	store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps("test"), ClusterUID: "native-cluster", Name: targetJournal.Name}
	scope := receiveJournalScope{Namespace: "test", MeshName: "mesh", MeshUID: target.MeshUID, MachineUID: target.MachineUID}
	run := func() ([]handover.PodReceipt, error) {
		return executeReceiveRetirement(ctx, api, store, scope, cfg, localKey, "agent", func(ctx context.Context, path string, a admittedReceiveRetirement) error {
			return retireAdmittedCloudNeighborLocked(ctx, api, devices, store, scope, cfg, localKey, path, a)
		})
	}
	receipts, failure := run()
	if failure == nil || !strings.Contains(failure.Error(), "remains in surviving cloud publication") || len(receipts) != 0 {
		t.Fatalf("stale adoption rejection: receipts=%d error=%v", len(receipts), failure)
	}
	live, err := devices.Device(cfg.iface)
	must(err)
	if len(live.Peers) != 2 {
		t.Fatal("stale publication changed native peers")
	}
	adoption.Data[tunnel.CloudPeersKey], err = json.Marshal(local.Desired)
	must(err)
	adoption.ResourceVersion = "2"
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
	if strings.Contains(string(cached), target.RootPublicKey) {
		t.Fatal("executable cache retained target")
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
				if len(serving.Plans) != 1 || serving.Withdrawn[target.MachineUID].RootPublicKey != target.RootPublicKey {
					return fmt.Errorf("survivor lost withdrawn ownership")
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
}

func cloudWithdrawalTestJournal(t *testing.T, plan tunnel.ReceiveTransitionPlan, uid string) *corev1.ConfigMap {
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
	transition, err := handover.NewTransition(spec, at, handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: time.Minute, DrainInterval: time.Second})
	must(err)
	for i := 0; i < 6; i++ {
		view, err := transition.View()
		must(err)
		if view.Complete {
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
