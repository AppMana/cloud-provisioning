package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Real kernel routes, durable recovery records and the local BGP RIB; the live
// admission callback is simulated. This does not qualify API authentication,
// packet continuity, external BGP convergence, physical reboot or AWS behavior.
func TestNativeFollowerServingRoutes(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_FOLLOWER_SERVING") != "1" {
		t.Skip("requires isolated guest namespaces")
	}
	requireStableAddressNamespace(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	key, err := wgtypes.GeneratePrivateKey()
	must(err)
	root, err := wgtypes.GeneratePrivateKey()
	must(err)
	site, err := wgtypes.GeneratePrivateKey()
	must(err)
	survivor, err := wgtypes.GeneratePrivateKey()
	must(err)
	plan := receiveRestoreTestPlan(t, root, site)
	cfg := config{iface: "no-tunnel", nodeName: "follower", secretNamespace: "test", secretName: "mesh", privateKeyFile: filepath.Join(t.TempDir(), "key"), routeTable: 518, fwmark: 518, bgpListenPort: -1}
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
		addr, err := netlink.ParseAddr(raw)
		must(err)
		addr.Flags = unix.IFA_F_NODAD
		must(netlink.AddrAdd(link, addr))
	}
	transit, err := tunnel.SiteTransit(mesh.Data, nil)
	must(err)
	must(installSiteTransit(cfg, transit, own))
	defer removeRouteRule(cfg.routeTable, cfg.fwmark)
	lock, err := lockReconciliation(cfg)
	must(err)
	defer lock.Close()
	expected := receiveInventoryCheckpoint{Version: 1, InventoryUID: "inventory", Inventory: tunnel.ReceiveInventory{MeshUID: plan.MeshUID}, NodeName: cfg.nodeName, NodeUID: "follower-uid", BootID: "boot", PublicKey: key.PublicKey().String()}
	peers, err := tunnel.SitePeers(mesh.Data)
	must(err)
	rootPublic := root.PublicKey().String()
	desired := map[string]map[string]string{rootPublic: {"172.29.0.1/32": "10.90.0.3", "fd29::1/128": "fd90::3"}}
	apply := func(check func() error) error {
		return applyReceiveFollowerRoutes(ctx, cfg, speaker, key, lock.Name(), expected, own, desired, peers, []tunnel.ReceiveTransitionPlan{plan}, check)
	}
	checkRoutes := func(want map[string]string) {
		t.Helper()
		local := cfg
		local.receiveRouteScope = map[string]bool{}
		local.receiveRelayRoutes = want
		var hosts []net.IPNet
		for raw := range want {
			local.receiveRouteScope[raw] = true
			_, dst, _ := net.ParseCIDR(raw)
			hosts = append(hosts, *dst)
		}
		must(verifyReceiveRouteInstallation(local, hosts, nil, own))
	}
	checkSurvivor := func() {
		t.Helper()
		checkRoutes(map[string]string{"172.29.0.3/32": "10.90.0.2", "fd29::3/128": "fd90::2"})
		if len(speaker.advertised) != 4 {
			t.Fatalf("advertisements lost: %+v", speaker.advertised)
		}
	}
	t.Run("reject-before-mutation", func(t *testing.T) {
		if err := apply(func() error { return errors.New("changed authority") }); err == nil {
			t.Fatal("changed authority accepted")
		}
		if _, err := os.Lstat(lock.Name() + ".follower.json"); !os.IsNotExist(err) {
			t.Fatal("failed admission staged routes")
		}
		checkRoutes(map[string]string{"172.29.0.1/32": "10.90.0.2", "fd29::1/128": "fd90::2"})
		checkSurvivor()
	})
	t.Run("interrupted-after-native", func(t *testing.T) {
		calls := 0
		if err := apply(func() error {
			calls++
			if calls == 3 {
				return errors.New("authority changed after native work")
			}
			return nil
		}); err == nil {
			t.Fatal("post-native race accepted")
		}
		checkRoutes(desired[rootPublic])
		checkSurvivor()
		pending, err := readReceiveFollowerCache(lock.Name()+".follower.json", cfg, expected, own)
		must(err)
		if len(pending.Routes[rootPublic]) != 2 {
			t.Fatal("interrupted route variants lost")
		}
	})
	t.Run("retry-and-restart", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			must(apply(func() error { return nil }))
			checkRoutes(desired[rootPublic])
			checkSurvivor()
		}
		saved, err := readReceiveFollowerCache(lock.Name()+".follower.json", cfg, expected, own)
		must(err)
		if len(saved.Routes[rootPublic]) != 1 || !reflect.DeepEqual(saved.Routes[rootPublic][0], desired[rootPublic]) {
			t.Fatal("verified selection not compacted")
		}
		if len(saved.Routes[survivor.PublicKey().String()]) != 1 {
			t.Fatal("omitted legacy worker lost durable ownership")
		}
		// A new admitted boot must reverify; the old record never selects a path.
		expected.BootID = "new-boot"
		must(apply(func() error { return nil }))
		saved, err = readReceiveFollowerCache(lock.Name()+".follower.json", cfg, expected, own)
		must(err)
		if saved.BootID != "new-boot" {
			t.Fatal("boot admission not recorded")
		}
	})
	t.Run("reject-foreign-native-gateway", func(t *testing.T) {
		_, dst, _ := net.ParseCIDR("172.29.0.1/32")
		bad := netlink.Route{Dst: dst, Gw: net.ParseIP("10.90.0.99"), Src: net.ParseIP(own[0]), LinkIndex: link.Attrs().Index, Table: cfg.routeTable, Protocol: unix.RTPROT_BOOT}
		must(netlink.RouteReplace(&bad))
		if err := apply(func() error { return nil }); err == nil {
			t.Fatal("unowned route overwritten")
		}
		checkRoutes(map[string]string{"172.29.0.1/32": "10.90.0.99"})
		bad.Gw = net.ParseIP("10.90.0.3")
		must(netlink.RouteReplace(&bad))
	})
	t.Run("superseded-destinations", func(t *testing.T) {
		// Model writes from an interrupted older candidate. The serving writer
		// must remove these only with recorded ownership and live admission.
		previous, err := readReceiveFollowerCache(lock.Name()+".follower.json", cfg, expected, own)
		must(err)
		extra := map[string]string{"172.29.0.9/32": "10.90.0.3", "fd29::9/128": "fd90::3"}
		_, err = stageReceiveFollowerRoutes(lock.Name()+".follower.json", cfg, expected, own, previous, map[string]map[string]string{rootPublic: extra})
		must(err)
		local := cfg
		local.receiveRouteScope = map[string]bool{}
		local.receiveRelayRoutes = extra
		var hosts []net.IPNet
		for raw := range extra {
			local.receiveRouteScope[raw] = true
			_, dst, err := net.ParseCIDR(raw)
			must(err)
			hosts = append(hosts, *dst)
			must(speaker.advertise(ctx, transitRoute{prefix: raw}, false))
			speaker.advertised[raw] = 0
		}
		must(installRoutes(local, hosts, nil, nil, nil, nil, own))
		must(apply(func() error { return nil }))
		checkRoutes(desired[rootPublic])
		checkSurvivor()
		local.receiveRelayRoutes = nil
		must(verifyReceiveRouteInstallation(local, nil, nil, own))
		// This readback checks the actual RIB as well as the speaker's cache.
		must(speaker.withdrawReceivePrefixes(ctx, local.receiveRouteScope))
	})
	t.Run("committed-plan-to-kernel", func(t *testing.T) {
		plan.Desired.Peers[0].RouteHosts = []string{"10.90.0.2", "fd90::2"}
		plan.Base, err = json.Marshal(plan.Desired)
		must(err)
		journal := cloudWithdrawalTestJournal(t, plan, "journal")
		api := fake.NewClientset(journal)
		store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps(plan.Namespace), Name: journal.Name, ClusterUID: "native-cluster"}
		snapshot, err := store.Load(ctx)
		must(err)
		admitted := receiveObserverAdmission{
			Inventory: tunnel.ReceiveInventory{ClusterUID: "native-cluster", MeshUID: plan.MeshUID, MachineUIDs: []string{plan.MachineUID}},
			Metadata:  metav1.ObjectMeta{UID: "inventory"},
			Journals:  map[string]handover.Snapshot{plan.MachineUID: snapshot}, Mesh: mesh,
			Node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: cfg.nodeName, UID: "follower-uid"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: own[0]}, {Type: corev1.NodeInternalIP, Address: own[1]},
			}}},
		}
		calls := 0
		must(reconcileReceiveFollower(ctx, api, cfg, speaker, key, lock.Name(), admitted, func() error { calls++; return nil }))
		if calls < 3 {
			t.Fatal("serving path did not renew live authorization")
		}
		checkRoutes(map[string]string{"172.29.0.1/32": "10.90.0.2", "fd29::1/128": "fd90::2"})
		checkSurvivor()
		t.Run("initial-storage", func(t *testing.T) {
			for _, fault := range []string{"fresh", "interrupted", "no-legacy-endpoint", "orphaned-routes", "orphaned-bgp", "receiver-state", "live-failure", "symlink"} {
				t.Run(fault, func(t *testing.T) {
					run := cfg
					runAdmitted := admitted
					if fault == "no-legacy-endpoint" {
						runAdmitted.Mesh = admitted.Mesh.DeepCopy()
						delete(runAdmitted.Mesh.Data, tunnel.NodeTunnelAddressPrefix+"site")
					}
					run.privateKeyFile = filepath.Join(t.TempDir(), "key")
					must(os.WriteFile(run.privateKeyFile, []byte(key.String()), 0600))
					runLock, err := lockReconciliation(run)
					must(err)
					defer runLock.Close()
					runSpeaker, err := startTransitSpeaker(ctx, -1, 64512, strings.Join(own, ","))
					must(err)
					defer runSpeaker.stop(ctx)
					if fault == "fresh" || fault == "interrupted" || fault == "no-legacy-endpoint" {
						must(withdrawReceiveDeviceRoutes(ctx, cfg, []map[string]receiveRouteSelection{{}}, own, map[string]string{"172.29.0.1/32": "10.90.0.2", "fd29::1/128": "fd90::2"}))
					}
					switch fault {
					case "orphaned-routes":
						must(os.WriteFile(runLock.Name()+".follower.json", []byte("{}"), 0600))
					case "orphaned-bgp":
						must(os.WriteFile(transitAdvertisementCachePath(run), []byte("{}"), 0600))
					case "receiver-state":
						must(os.WriteFile(siteReceiveCachePath(run), []byte("{}"), 0600))
					case "symlink":
						target := filepath.Join(t.TempDir(), "foreign")
						must(os.WriteFile(target, []byte("untouched"), 0600))
						must(os.Symlink(target, siteForwardingCachePath(run)))
					}
					calls := 0
					check := func() error {
						calls++
						if fault == "live-failure" || (fault == "interrupted" && calls == 2) {
							return errors.New("live authority changed")
						}
						return nil
					}
					err = reconcileReceiveFollower(ctx, api, run, runSpeaker, key, runLock.Name(), runAdmitted, check)
					if fault != "fresh" && fault != "interrupted" && fault != "no-legacy-endpoint" {
						if err == nil {
							t.Fatal("unsafe initial storage accepted")
						}
						if len(runSpeaker.advertised) != 0 {
							t.Fatal("rejected initialization advertised routes")
						}
						return
					}
					if fault == "interrupted" {
						if err == nil {
							t.Fatal("interrupted initialization succeeded")
						}
						if _, err := os.Lstat(transitAdvertisementCachePath(run)); !os.IsNotExist(err) {
							t.Fatal("interrupted initialization saved BGP metadata")
						}
						empty, err := readSiteForwardingCache(run, key)
						must(err)
						peers, err := tunnel.SitePeers(empty.Data)
						must(err)
						if len(peers) != 0 {
							t.Fatal("initial metadata claimed uninstalled worker routes")
						}
						absent := cfg
						absent.receiveRouteScope = map[string]bool{"172.29.0.1/32": true, "fd29::1/128": true}
						must(verifyReceiveRouteInstallation(absent, nil, nil, own))
					} else {
						must(err)
					}
					must(reconcileReceiveFollower(ctx, api, run, runSpeaker, key, runLock.Name(), runAdmitted, func() error { return nil }))
					cached, empty, err := readTransitAdvertisements(run, runSpeaker)
					must(err)
					oldPeers, err := tunnel.SitePeers(empty.Data)
					must(err)
					if len(oldPeers) != 0 || len(cached.Peers) != 0 || len(runSpeaker.advertised) != 2 {
						t.Fatal("initial storage or advertisements claimed unrelated forwarding")
					}
					readScope := expected
					readScope.BootID, err = localReceiveBootID()
					must(err)
					record, err := readReceiveFollowerCache(runLock.Name()+".follower.json", run, readScope, own)
					must(err)
					if len(record.Routes) != 1 || len(record.Routes[rootPublic]) != 1 {
						t.Fatal("initial serving did not record exact per-worker ownership")
					}
					checkRoutes(map[string]string{"172.29.0.1/32": "10.90.0.2", "fd29::1/128": "fd90::2"})
					checkSurvivor()
				})
			}
		})
		t.Run("ordinary-lifecycle", func(t *testing.T) {
			verifyNativeOrdinaryFollower(t, api, cfg, key, lock.Name(), speaker, admitted, plan)
		})
		admitted.Node.Status.Addresses[0].Address = "10.90.0.99"
		if err := reconcileReceiveFollower(ctx, api, cfg, speaker, key, lock.Name(), admitted, func() error { t.Fatal("changed source reached mutation admission"); return nil }); err == nil {
			t.Fatal("changed Node source accepted")
		}
	})
}
