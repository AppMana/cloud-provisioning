package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Guest-only native integration: real routes, BGP RIB and paired caches through
// the bounded locked executor; simulated Kubernetes API and terminal journal.
// No packet continuity, external convergence or token authentication claim.
// Former endpoints with receiver state and unavailable underlays remain explicit
// rejection cases, not qualified recovery paths.
func TestNativeCombinedFollowerWithdrawal(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_COMBINED_FOLLOWER") != "1" {
		t.Skip("requires isolated guest namespaces")
	}
	for _, recorded := range []bool{false, true} {
		name := "legacy-cache"
		if recorded {
			name = "interrupted-worker-routes"
		}
		t.Run(name, func(t *testing.T) { nativeCombinedFollowerWithdrawal(t, recorded) })
	}
}

func nativeCombinedFollowerWithdrawal(t *testing.T, withRecordedRoutes bool) {
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
	key, _ := wgtypes.GeneratePrivateKey()
	root, _ := wgtypes.GeneratePrivateKey()
	site, _ := wgtypes.GeneratePrivateKey()
	survivor, _ := wgtypes.GeneratePrivateKey()
	plan := receiveRestoreTestPlan(t, root, site)
	journal := cloudWithdrawalTestJournal(t, plan, "target-journal")
	cfg := config{iface: "no-tunnel", nodeName: "follower", secretNamespace: "test", secretName: "mesh", privateKeyFile: filepath.Join(t.TempDir(), "key"), routeTable: 518, fwmark: 518, bgpListenPort: -1, receiptPodName: "agent", receiptPodUID: "agent-uid"}
	must(os.WriteFile(cfg.privateKeyFile, []byte(key.String()), 0600))
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "mesh", UID: "restore-mesh", ResourceVersion: "1"}, Data: map[string][]byte{
		tunnel.NodePublicKeyPrefix + "site": []byte(site.PublicKey().String()), tunnel.NodeTunnelAddressPrefix + "site": []byte("10.90.0.2/24"), tunnel.NodeAddressesPrefix + "site": []byte("10.90.0.2,fd90::2"), tunnel.SiteAddressesPrefix + "follower": []byte("10.90.0.4,fd90::4"),
		tunnel.PeerPublicKeyPrefix + "worker": []byte(root.PublicKey().String()), tunnel.PeerEndpointPrefix + "worker": []byte("198.51.100.1:51820"), tunnel.PeerAllowedIPsPrefix + "worker": []byte("172.29.0.1/32,fd29::1/128"), tunnel.PeerRouteHostsPrefix + "worker": []byte("172.29.0.1,fd29::1"),
		tunnel.PeerPublicKeyPrefix + "survivor": []byte(survivor.PublicKey().String()), tunnel.PeerEndpointPrefix + "survivor": []byte("198.51.100.3:51820"), tunnel.PeerAllowedIPsPrefix + "survivor": []byte("172.29.0.3/32,fd29::3/128"), tunnel.PeerRouteHostsPrefix + "survivor": []byte("172.29.0.3,fd29::3"),
	}}
	own := []string{"10.90.0.4", "fd90::4"}
	must(saveSiteForwardingCache(cfg, key, mesh, nil, map[string]bool{}, own))
	speaker, err := startTransitSpeaker(ctx, -1, 64512, strings.Join(own, ","))
	must(err)
	defer speaker.stop(ctx)
	must(saveTransitAdvertisements(cfg, speaker, mesh, []string{}))
	ads, _, err := transitAdvertisementPlan(mesh.Data, cfg.nodeName)
	must(err)
	must(speaker.reconcile(ctx, nil, ads))
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "underlay"}}
	must(netlink.LinkAdd(link))
	defer netlink.LinkDel(link)
	must(netlink.LinkSetUp(link))
	for _, raw := range []string{"10.90.0.4/24", "fd90::4/64"} {
		address, e := netlink.ParseAddr(raw)
		must(e)
		address.Flags = unix.IFA_F_NODAD
		must(netlink.AddrAdd(link, address))
	}
	transit, err := tunnel.SiteTransit(mesh.Data, nil)
	must(err)
	must(installSiteTransit(cfg, transit, own))
	defer removeRouteRule(cfg.routeTable, cfg.fwmark)
	intent := attachment.RemoteWithdrawalIntent{MeshNamespace: "test", MeshName: "mesh", MeshUID: plan.MeshUID, SourceVersion: "1", MachineName: plan.MachineName, MachineUID: plan.MachineUID, PublicKey: plan.RootPublicKey, DrainIntent: "native-test", Consumers: []attachment.ConsumerTarget{
		{Site: true, NodeName: "site", NodeUID: "site-uid", PublicKey: site.PublicKey().String()},
		{Site: true, NodeName: "follower", NodeUID: "follower-uid"},
		{NodeName: "survivor", NodeUID: "survivor-uid", MachineName: "survivor", SecretUID: "survivor-adoption", TunnelAddress: "172.29.0.3", PublicKey: survivor.PublicKey().String()},
	}}
	retirement, err := json.Marshal(map[string]any{"version": 1, "journalSHA256": tunnel.HashPeerList([]byte(journal.Data["transition.json"])), "withdrawal": intent})
	must(err)
	inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: "native-cluster", Namespace: "test", MeshName: "mesh", MeshUID: plan.MeshUID, MachineUIDs: []string{plan.MachineUID}, Publications: map[string]tunnel.ReceiveJournalPublication{plan.MachineUID: {JournalUID: string(journal.UID), Retirement: retirement}}}
	inventoryRaw, err := json.Marshal(inventory)
	must(err)
	mesh.Data, _, err = tunnel.WithdrawRemotePeer(mesh.Data, plan.MachineName, plan.RootPublicKey)
	must(err)
	mesh.ResourceVersion = "2"
	boot, err := localReceiveBootID()
	must(err)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "follower", UID: "follower-uid", ResourceVersion: "1"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: boot}, Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: own[0]}, {Type: corev1.NodeInternalIP, Address: own[1]}}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "agent", UID: "agent-uid"}, Spec: corev1.PodSpec{NodeName: "follower"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "dialer", Ready: true, ContainerID: "agent", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}}}}}
	api := fake.NewClientset(mesh, journal, node, pod, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: tunnel.ReceiveInventoryName(plan.MeshUID), UID: "inventory", ResourceVersion: "1"}, Data: map[string]string{"inventory.json": string(inventoryRaw)}})
	store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps("test"), ClusterUID: "native-cluster", Name: journal.Name}
	scope := receiveJournalScope{Namespace: "test", MeshName: "mesh", MeshUID: plan.MeshUID, MachineUID: plan.MachineUID}
	followerPath := filepath.Join(filepath.Dir(cfg.privateKeyFile), cfg.iface+".reconcile.lock.follower.json")
	followerScope := receiveInventoryCheckpoint{Version: 1, InventoryUID: "inventory", Inventory: inventory, NodeName: cfg.nodeName, NodeUID: string(node.UID), BootID: boot, PublicKey: key.PublicKey().String()}
	var pending receiveFollowerCache
	if withRecordedRoutes {
		recorded, err := stageReceiveFollowerRoutes(followerPath, cfg, followerScope, own, receiveFollowerCache{}, map[string]map[string]string{
			plan.RootPublicKey:            {"172.29.0.1/32": "10.90.0.2", "fd29::1/128": "fd90::2"},
			survivor.PublicKey().String(): {"172.29.0.3/32": "10.90.0.2", "fd29::3/128": "fd90::2"},
		})
		must(err)
		// Simulate a write interrupted before compaction: the recorded candidate
		// relay differs from the older whole-mesh cache's election.
		pending, err = stageReceiveFollowerRoutes(followerPath, cfg, followerScope, own, recorded, map[string]map[string]string{
			plan.RootPublicKey: {"172.29.0.1/32": "10.90.0.3", "fd29::1/128": "fd90::3"},
		})
		must(err)
		for raw, gateway := range map[string]string{"172.29.0.1/32": "10.90.0.3", "fd29::1/128": "fd90::3"} {
			_, dst, err := net.ParseCIDR(raw)
			must(err)
			must(netlink.RouteReplace(&netlink.Route{Dst: dst, LinkIndex: link.Attrs().Index, Gw: net.ParseIP(gateway), Src: transitSource(dst.IP, own, []net.IP{net.ParseIP(own[0]), net.ParseIP(own[1])}), Table: cfg.routeTable, Protocol: unix.RTPROT_BOOT, Priority: receiveRoutePriority(dst)}))
		}
	}
	run := func() ([]handover.PodReceipt, error) {
		return executeReceiveRetirement(ctx, api, store, scope, cfg, key, "agent", func(ctx context.Context, path string, a admittedReceiveRetirement) error {
			return retireAdmittedFollowerLocked(ctx, api, speaker, store, scope, cfg, key, path, a)
		})
	}
	// A former endpoint cannot acknowledge follower-only cleanup while executable
	// receiver state remains. The injected marker is removed only by the fixture.
	marker := siteReceiveCachePath(cfg)
	must(os.WriteFile(marker, []byte("{}"), 0600))
	receipts, failure := run()
	if failure == nil || !strings.Contains(failure.Error(), "unresolved receiver cache") || len(receipts) != 0 {
		t.Fatalf("receiver evidence was ignored: %v", failure)
	}
	for _, raw := range []string{"172.29.0.1/32", "fd29::1/128"} {
		_, err := advertisedMED(ctx, speaker, raw)
		must(err)
	}
	must(os.Remove(marker))
	// A live survivor can change after the cached projection was written.
	// Its newly published overlap must still block withdrawal before BGP or
	// native forwarding is touched.
	mesh.Data[tunnel.PeerAllowedIPsPrefix+"survivor"] = []byte("172.29.0.0/24,fd29::/64")
	mesh.ResourceVersion = "3"
	_, err = api.CoreV1().Secrets("test").Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	receipts, failure = run()
	if failure == nil || len(receipts) != 0 {
		t.Fatal("live survivor overlap authorized cleanup")
	}
	for _, raw := range []string{"172.29.0.1/32", "fd29::1/128"} {
		_, err := advertisedMED(ctx, speaker, raw)
		must(err)
	}
	mesh.Data[tunnel.PeerAllowedIPsPrefix+"survivor"] = []byte("172.29.0.3/32,fd29::3/128")
	mesh.ResourceVersion = "4"
	_, err = api.CoreV1().Secrets("test").Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	for i := 0; i < 2; i++ {
		receipts, err := run()
		must(err)
		if len(receipts) != 1 {
			t.Fatal("missing follower observation")
		}
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
	must(err)
	if len(routes) != 2 {
		t.Fatal("unexpected remaining routes", routes)
	}
	for _, r := range routes {
		if r.Dst == nil || (r.Dst.String() != "172.29.0.3/32" && r.Dst.String() != "fd29::3/128") || r.LinkIndex != link.Attrs().Index || r.Protocol != unix.RTPROT_BOOT || !r.Gw.Equal(transit.Gateway(r.Dst.IP)) || !r.Src.Equal(transitSource(r.Dst.IP, own, []net.IP{net.ParseIP(own[0]), net.ParseIP(own[1])})) {
			t.Fatal("survivor route changed", r)
		}
	}
	must(verifyReceivePolicyRules(cfg))
	for _, raw := range []string{"172.29.0.1/32", "fd29::1/128"} {
		if _, err := advertisedMED(ctx, speaker, raw); err == nil {
			t.Fatal("departed BGP destination remains")
		}
	}
	for _, a := range ads {
		if a.prefix == "172.29.0.3/32" || a.prefix == "fd29::3/128" {
			med, err := advertisedMED(ctx, speaker, a.prefix)
			must(err)
			if med != a.med {
				t.Fatal("survivor preference changed")
			}
		}
	}
	_, saved, err := readTransitAdvertisements(cfg, speaker)
	must(err)
	if len(saved.Data[tunnel.PeerPublicKeyPrefix+"worker"]) != 0 || string(saved.Data[tunnel.PeerPublicKeyPrefix+"survivor"]) != survivor.PublicKey().String() {
		t.Fatal("cache cleanup lost survivor or retained target")
	}
	if withRecordedRoutes {
		remaining, err := readReceiveFollowerCache(followerPath, cfg, followerScope, own)
		must(err)
		if len(remaining.Routes) != 1 || len(remaining.Routes[survivor.PublicKey().String()]) != 1 {
			t.Fatal("follower route ownership retained the retired worker or lost the survivor")
		}
		before, err := os.ReadFile(followerPath)
		must(err)
		if _, err := stageReceiveFollowerRoutes(followerPath, cfg, followerScope, own, pending, map[string]map[string]string{plan.RootPublicKey: pending.Routes[plan.RootPublicKey][0]}); err == nil {
			t.Fatal("stale pre-retirement write restored departed route ownership")
		}
		after, err := os.ReadFile(followerPath)
		must(err)
		if string(before) != string(after) {
			t.Fatal("rejected stale write changed post-retirement ownership")
		}
	}
}
