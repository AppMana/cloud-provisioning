package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
	"k8s.io/client-go/kubernetes/fake"
)

// Disposable guest mount/network namespaces only. Actual WireGuard, route,
// cache and BGP cleanup through the locked executor; API history is synthetic.
// No external BGP convergence, traffic continuity, token authentication, source
// exception/relay variants or cloud lifecycle qualification is implied.
func TestNativeCombinedSiteWithdrawal(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_COMBINED_WITHDRAWAL") != "1" {
		t.Skip("requires isolated guest namespaces")
	}
	for _, phase := range []string{"", "prepare", "switch", "drain", "retire", "abort"} {
		name := phase
		if name == "" {
			name = "completed"
		}
		t.Run(name, func(t *testing.T) { nativeCombinedSiteWithdrawal(t, phase) })
	}
}

// Each exact case must run in its own guest namespace. Two withdrawals share
// captured authority; failure on the second must suppress the joint receipt even
// after the first kernel cleanup succeeded. API history remains synthetic.
func TestNativeJointSiteWithdrawal(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_COMBINED_WITHDRAWAL") != "1" {
		t.Skip("requires isolated guest namespaces")
	}
	for _, phase := range []string{"", "prepare", "switch", "drain", "retire", "abort"} {
		name := phase
		if name == "" {
			name = "completed"
		}
		t.Run(name, func(t *testing.T) { nativeCombinedSiteWithdrawal(t, phase, true) })
	}
}

func nativeCombinedSiteWithdrawal(t *testing.T, phase string, joint ...bool) {
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
	root, _ := wgtypes.GeneratePrivateKey()
	survivor, _ := wgtypes.GeneratePrivateKey()
	plan := receiveRestoreTestPlan(t, root, site)
	cfg := config{iface: "withdrawsite", nodeName: "site", secretNamespace: "test", secretName: "mesh", privateKeyFile: filepath.Join(t.TempDir(), "key"), routeTable: 518, fwmark: 518, mtu: 1400, bgpListenPort: -1, receiptPodName: "agent", receiptPodUID: "agent-uid"}
	must(os.WriteFile(cfg.privateKeyFile, []byte(site.String()), 0600))
	boot, err := localReceiveBootID()
	must(err)
	plans := []tunnel.ReceiveTransitionPlan{plan}
	roots := []wgtypes.Key{root}
	if len(joint) != 0 && joint[0] {
		second, secondRoot := secondNativeWithdrawal(t, site)
		plans, roots = append(plans, second), append(roots, secondRoot)
	}
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "mesh", UID: "restore-mesh", ResourceVersion: "1"}, Data: map[string][]byte{
		tunnel.NodePublicKeyPrefix + "site": []byte(site.PublicKey().String()), tunnel.NodeTunnelAddressPrefix + "site": []byte("10.90.0.2/24"), tunnel.NodeTunnelAddress6Prefix + "site": []byte("fd90::2/64"), tunnel.NodeAddressesPrefix + "site": []byte("10.90.0.2,fd90::2"),
		tunnel.PeerPublicKeyPrefix + "survivor": []byte(survivor.PublicKey().String()), tunnel.PeerEndpointPrefix + "survivor": []byte("198.51.100.3:51820"), tunnel.PeerAllowedIPsPrefix + "survivor": []byte("172.29.0.3/32,fd29::3/128"), tunnel.PeerRouteHostsPrefix + "survivor": []byte("172.29.0.3,fd29::3"),
	}}
	cached := map[string]bool{"survivor": true}
	for _, member := range plans {
		addNativeWithdrawalPeer(mesh, member)
		cached[member.MachineName] = true
	}
	must(saveSiteForwardingCache(cfg, site, mesh, nil, cached, nil))
	speaker, err := startTransitSpeaker(ctx, -1, 64512, "10.90.0.2,fd90::2")
	must(err)
	defer speaker.stop(ctx)
	must(saveTransitAdvertisements(cfg, speaker, mesh, []string{}))
	routes, _, err := transitAdvertisementPlan(mesh.Data, "site")
	must(err)
	must(speaker.reconcile(ctx, nil, routes))
	consumers := []attachment.ConsumerTarget{{Site: true, NodeName: "site", NodeUID: "site-uid", PublicKey: site.PublicKey().String()}, {NodeName: "survivor", NodeUID: "survivor-uid", MachineName: "survivor", SecretUID: "survivor-adoption", TunnelAddress: "172.29.0.3", PublicKey: survivor.PublicKey().String()}}
	inventory, objects := nativeWithdrawalJournals(t, mesh, plans, consumers, phase)
	inventoryRaw, err := json.Marshal(inventory)
	must(err)
	for _, member := range plans {
		mesh.Data, _, err = tunnel.WithdrawRemotePeer(mesh.Data, member.MachineName, member.RootPublicKey)
		must(err)
	}
	mesh.ResourceVersion = "2"
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "site", UID: "site-uid", ResourceVersion: "1"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: boot}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "agent", UID: "agent-uid"}, Spec: corev1.PodSpec{NodeName: "site"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "dialer", Ready: true, ContainerID: "agent", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}}}}}
	objects = append(objects, mesh, node, pod, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: tunnel.ReceiveInventoryName(plan.MeshUID), UID: "inventory", ResourceVersion: "1"}, Data: map[string]string{"inventory.json": string(inventoryRaw)}})
	api := fake.NewClientset(objects...)
	devices, err := wgctrl.New()
	must(err)
	defer devices.Close()
	legacy := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: cfg.iface}}
	must(netlink.LinkAdd(legacy))
	defer netlink.LinkDel(legacy)
	must(netlink.LinkSetUp(legacy))
	must(ensureRouteRule(cfg.routeTable, cfg.fwmark))
	defer removeRouteRule(cfg.routeTable, cfg.fwmark)
	for _, address := range []string{"10.90.0.2/24", "fd90::2/64"} {
		a, e := netlink.ParseAddr(address)
		must(e)
		must(netlink.AddrAdd(legacy, a))
	}
	allowed := func(raw string) net.IPNet { _, prefix, e := net.ParseCIDR(raw); must(e); return *prefix }
	peers := []wgtypes.PeerConfig{{PublicKey: survivor.PublicKey(), AllowedIPs: []net.IPNet{allowed("172.29.0.3/32"), allowed("fd29::3/128")}}}
	interfaces := []string{}
	receivers := []*netlink.Wireguard{}
	retiredIPs := []string{}
	addRoute := func(raw string, link int) {
		dst := allowed(raw)
		src := net.ParseIP("10.90.0.2")
		if dst.IP.To4() == nil {
			src = net.ParseIP("fd90::2")
		}
		must(netlink.RouteAdd(&netlink.Route{LinkIndex: link, Dst: &dst, Table: cfg.routeTable, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT, Src: src, Priority: receiveRoutePriority(&dst)}))
	}
	for i, member := range plans {
		addresses := []string{fmt.Sprintf("172.29.0.%d/32", i+1), fmt.Sprintf("fd29::%d/128", i+1)}
		ips := []net.IPNet{allowed(addresses[0]), allowed(addresses[1])}
		peers = append(peers, wgtypes.PeerConfig{PublicKey: roots[i].PublicKey(), AllowedIPs: ips})
		selections, err := receivePhaseSelections(site, member, "site-uid")
		must(err)
		selected := selections[0]
		receiver := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: selected.Interface, Alias: selected.Alias}}
		must(netlink.LinkAdd(receiver))
		must(netlink.LinkSetAlias(receiver, selected.Alias))
		must(netlink.LinkSetUp(receiver))
		remote, err := wgtypes.ParseKey(selected.PeerPublicKey)
		must(err)
		must(devices.ConfigureDevice(selected.Interface, wgtypes.Config{PrivateKey: &site, FirewallMark: &cfg.fwmark, Peers: []wgtypes.PeerConfig{{PublicKey: remote, AllowedIPs: ips}}}))
		for _, raw := range addresses {
			addRoute(raw, receiver.Attrs().Index)
		}
		interfaces, receivers = append(interfaces, selected.Interface), append(receivers, receiver)
		retiredIPs = append(retiredIPs, addresses...)
	}
	must(devices.ConfigureDevice(cfg.iface, wgtypes.Config{PrivateKey: &site, FirewallMark: &cfg.fwmark, Peers: peers}))
	for _, raw := range []string{"172.29.0.3/32", "fd29::3/128"} {
		addRoute(raw, legacy.Attrs().Index)
	}
	store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps("test"), ClusterUID: "native-cluster", Name: tunnel.ReceiveTransitionName(plan.MachineUID)}
	scope := receiveJournalScope{Namespace: "test", MeshName: "mesh", MeshUID: plan.MeshUID, MachineUID: plan.MachineUID}
	// A foreign-table dependency must block native completion. Removing the
	// injected dependency permits the same persisted withdrawal to retry.
	blockedDst := allowed("198.19.0.1/32")
	dependency := netlink.Route{LinkIndex: receivers[len(receivers)-1].Attrs().Index, Dst: &blockedDst, Table: 777, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_STATIC}
	must(netlink.RouteAdd(&dependency))
	rejected, failure := executeReceiveRetirement(ctx, api, store, scope, cfg, site, "agent", func(ctx context.Context, path string, a admittedReceiveRetirement) error {
		memberStore, memberScope := retirementMemberScope(store, scope, a, a.Withdrawal.MachineUID)
		return retireAdmittedSiteReceiversLocked(ctx, api, devices, speaker, memberStore, memberScope, cfg, site, path, a)
	})
	if failure == nil || len(rejected) != 0 {
		t.Fatal("foreign dependency authorized completion")
	}
	if _, err := devices.Device(interfaces[len(interfaces)-1]); err != nil {
		t.Fatal("rejected cleanup removed blocked receiver", err, failure)
	}
	if len(plans) > 1 {
		if _, err := netlink.LinkByName(interfaces[0]); !isLinkNotFound(err) {
			t.Fatal("first cleanup did not complete before second failed", err, failure)
		}
	}
	must(netlink.RouteDel(&dependency))
	for i := 0; i < 2; i++ {
		receipts, e := executeReceiveRetirement(ctx, api, store, scope, cfg, site, "agent", func(ctx context.Context, path string, a admittedReceiveRetirement) error {
			memberStore, memberScope := retirementMemberScope(store, scope, a, a.Withdrawal.MachineUID)
			return retireAdmittedSiteReceiversLocked(ctx, api, devices, speaker, memberStore, memberScope, cfg, site, path, a)
		})
		must(e)
		if len(receipts) != 1 {
			t.Fatal("missing native observation")
		}
	}
	for _, name := range interfaces {
		if _, err := netlink.LinkByName(name); !isLinkNotFound(err) {
			t.Fatal("receiver remains", err)
		}
	}
	remaining, err := devices.Device(cfg.iface)
	must(err)
	if len(remaining.Peers) != 1 || remaining.Peers[0].PublicKey != survivor.PublicKey() {
		t.Fatal("peer cleanup lost survivor or retained target")
	}
	for _, raw := range retiredIPs {
		if _, err := advertisedMED(ctx, speaker, raw); err == nil {
			t.Fatal("deleted advertisement remains")
		}
	}
	for _, raw := range []string{"172.29.0.3/32", "fd29::3/128"} {
		if _, err := advertisedMED(ctx, speaker, raw); err != nil {
			t.Fatal("survivor advertisement missing", err)
		}
	}
	installed, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
	must(err)
	if len(installed) != 2 {
		t.Fatal("unexpected routes after cleanup", installed)
	}
	for _, route := range installed {
		if route.Dst == nil || (route.Dst.String() != "172.29.0.3/32" && route.Dst.String() != "fd29::3/128") || route.LinkIndex != legacy.Attrs().Index || route.Protocol != unix.RTPROT_BOOT || route.Priority != receiveRoutePriority(route.Dst) {
			t.Fatal("survivor route changed", route)
		}
	}
	must(verifyReceivePolicyRules(cfg))
	for _, route := range routes {
		if route.prefix == "172.29.0.3/32" || route.prefix == "fd29::3/128" {
			med, err := advertisedMED(ctx, speaker, route.prefix)
			must(err)
			if med != route.med {
				t.Fatal("survivor BGP preference changed")
			}
		}
	}
	_, saved, err := readTransitAdvertisements(cfg, speaker)
	must(err)
	for _, member := range plans {
		if len(saved.Data[tunnel.PeerPublicKeyPrefix+member.MachineName]) != 0 {
			t.Fatal("deleted cache entry remains")
		}
	}
}
