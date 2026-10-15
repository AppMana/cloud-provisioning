package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Run only in a fresh lab VM network namespace. An external Unix-socket TCP
// relay provides access to the real API without adding guest routes or veths.
// The staged kubeconfig contains a short-lived, narrowly scoped test identity.
func TestNativeReceiveRealAPI(t *testing.T) {
	kubeconfig := os.Getenv("CLDT_NATIVE_RECEIVE_API_CONFIG")
	if kubeconfig == "" {
		t.Skip("requires isolated lab API fixture")
	}
	socket := os.Getenv("CLDT_NATIVE_RECEIVE_API_SOCKET")
	namespace := os.Getenv("CLDT_NATIVE_RECEIVE_API_NAMESPACE")
	nodeUID := os.Getenv("CLDT_NATIVE_RECEIVE_API_NODE_UID")
	if socket == "" || !strings.HasPrefix(namespace, "cldt-receiver-api-") || nodeUID == "" {
		t.Fatal("incomplete isolated API fixture")
	}
	self, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	init, err := os.Readlink("/proc/1/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if self == init {
		t.Fatal("refusing guest host network namespace")
	}
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.Attrs().Name != "lo" {
			t.Fatal("namespace is not empty")
		}
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	apiConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal("loading test API identity failed")
	}
	apiConfig.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	apiConfig.Timeout = 10 * time.Second
	worker, err := kubernetes.NewForConfig(apiConfig)
	if err != nil {
		t.Fatal(err)
	}
	secrets := worker.CoreV1().Secrets(namespace)
	if _, err := secrets.Get(ctx, "unrelated", metav1.GetOptions{}); !apierrors.IsForbidden(err) {
		t.Fatal("unrelated Secret access was not denied")
	}
	if _, err := worker.CoreV1().Nodes().Get(ctx, "w1", metav1.GetOptions{}); !apierrors.IsForbidden(err) {
		t.Fatal("unrelated Node access was not denied")
	}
	node, err := worker.CoreV1().Nodes().Get(ctx, "cp", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(node.UID) != nodeUID {
		t.Fatal("lab Node identity changed")
	}
	root, _ := wgtypes.GeneratePrivateKey()
	oldPeer, _ := wgtypes.GeneratePrivateKey()
	newPeer, _ := wgtypes.GeneratePrivateKey()
	base := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: oldPeer.PublicKey().String(), Endpoint: "127.0.0.1:51822", WGAllowedIPs: []string{"10.90.0.10/32"}, RouteHosts: []string{"10.90.0.10"}}}}
	baseRaw, _ := json.Marshal(base)
	cfg := config{iface: "cldtapibase", peersFile: filepath.Join(t.TempDir(), "identity.json"), peersSecretNamespace: namespace, peersSecretName: "worker-tunnel-peers", nodeName: "cp", mtu: 1400, routeTable: 518, fwmark: 518, pollInterval: time.Second}
	bootstrap := tunnel.PeersFileDoc{PrivateKey: root.String(), LocalAddress: "10.88.0.3/32", Peers: base.Peers}
	raw, _ := json.Marshal(bootstrap)
	if err := os.WriteFile(cfg.peersFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	patchData := func(data map[string][]byte) {
		t.Helper()
		current, err := secrets.Get(ctx, "worker-tunnel-peers", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": current.UID, "resourceVersion": current.ResourceVersion}, "data": data})
		if _, err := secrets.Patch(ctx, "worker-tunnel-peers", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	patchData(map[string][]byte{tunnel.CloudPeersKey: baseRaw})
	devices, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer devices.Close()
	if err := reconcile(ctx, worker, devices, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := secrets.Get(ctx, "worker-tunnel-peers", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if before.Annotations[tunnel.AppliedListAnnotation] != tunnel.HashPeerList(baseRaw) {
		t.Fatal("base was not applied")
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	mesh, err := secrets.Get(ctx, "mesh", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	meshPatch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": mesh.UID, "resourceVersion": mesh.ResourceVersion}, "data": map[string][]byte{
		tunnel.NodePublicKeyPrefix + "cp":      []byte(newPeer.PublicKey().String()),
		tunnel.PeerPublicKeyPrefix + "worker":  []byte(root.PublicKey().String()),
		tunnel.PeerEndpointPrefix + "worker":   []byte("198.51.100.10:51820"),
		tunnel.PeerAllowedIPsPrefix + "worker": []byte("10.88.0.3/32"),
		tunnel.PeerRouteHostsPrefix + "worker": []byte("10.88.0.3"),
	}})
	mesh, err = secrets.Patch(ctx, "mesh", types.MergePatchType, meshPatch, metav1.PatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	request := tunnel.ReceivePrepare{Version: 1, MeshUID: string(mesh.UID), NodeName: "cp", NodeUID: nodeUID, BootID: node.Status.NodeInfo.BootID, RootPublicKey: root.PublicKey().String(), BaseHash: tunnel.HashPeerList(baseRaw), Ports: map[string]int{oldPeer.PublicKey().String(): 51821, newPeer.PublicKey().String(): 51822}, Desired: tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: newPeer.PublicKey().String(), WGAllowedIPs: []string{"10.90.0.10/32"}}}}, Native: []netip.Prefix{netip.MustParsePrefix("172.29.0.0/24")}}
	requestRaw, _ := json.Marshal(request)
	patchData(map[string][]byte{tunnel.ReceivePrepareKey: requestRaw})
	if err := reconcile(ctx, worker, devices, cfg); err != nil {
		t.Fatal(err)
	}
	current, err := secrets.Get(ctx, "worker-tunnel-peers", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var receipt tunnel.ReceiveReady
	if err := json.Unmarshal(current.Data[tunnel.ReceiveReadyKey], &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.RequestHash != tunnel.HashPeerList(requestRaw) || receipt.SecretUID != string(current.UID) || receipt.NodeUID != nodeUID || receipt.BootID != node.Status.NodeInfo.BootID || len(receipt.Bindings) != 2 {
		t.Fatal("real API receipt identity or binding mismatch")
	}
	target := tunnel.ReceiveTarget{MeshUID: request.MeshUID, SecretUID: string(current.UID), NodeName: node.Name, NodeUID: string(node.UID), BootID: node.Status.NodeInfo.BootID, RootPublicKey: root.PublicKey().String()}
	for _, condition := range node.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" && node.DeletionTimestamp == nil {
			target.Ready = true
		}
	}
	if _, err := tunnel.ValidateReceiveReadiness(target, baseRaw, requestRaw, current.Data[tunnel.ReceiveReadyKey]); err != nil {
		t.Fatal(err)
	}
	oldBootTarget := target
	oldBootTarget.BootID = "another-boot"
	if _, err := tunnel.ValidateReceiveReadiness(oldBootTarget, baseRaw, requestRaw, current.Data[tunnel.ReceiveReadyKey]); err == nil {
		t.Fatal("readiness survived a changed target boot identity")
	}
	if string(current.Data[tunnel.CloudPeersKey]) != string(baseRaw) || current.Annotations[tunnel.AppliedListAnnotation] != before.Annotations[tunnel.AppliedListAnnotation] {
		t.Fatal("preparation changed active forwarding document")
	}
	live, err := devices.Device(cfg.iface)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.Peers) != 1 || live.Peers[0].PublicKey != oldPeer.PublicKey() {
		t.Fatal("preparation changed active WireGuard peer")
	}
	// Consume the real cloud receipt through the site reconciliation path,
	// retaining the active cloud device and all existing routes.
	siteCfg := config{iface: "cldtapisite", privateKeyFile: filepath.Join(t.TempDir(), "site.key"), secretNamespace: namespace, secretName: "mesh", nodeName: "cp", mtu: 1400, fwmark: 518}
	if err := os.WriteFile(siteCfg.privateKeyFile, []byte(newPeer.String()), 0600); err != nil {
		t.Fatal(err)
	}
	var sitePlan tunnel.SiteReceiveClient
	for pass := 0; pass < 2; pass++ {
		if err := reconcileDurableSiteReceivers(ctx, worker, devices, siteCfg, mesh, newPeer); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(siteReceiveCachePath(siteCfg))
		if err != nil {
			t.Fatal(err)
		}
		cached, err := decodeSiteReceiveCache(raw, siteCfg, newPeer)
		if err != nil || len(cached.Bindings) != 1 {
			t.Fatal("site preparation not durable", err)
		}
		prepared, err := tunnel.PlanSiteReceiveClient(cached.MeshUID, cached.PublicKey, root.PublicKey().String(), cached.Bindings[root.PublicKey().String()], cached.Peers, cached.Peers)
		if err != nil {
			t.Fatal(err)
		}
		if pass == 1 && !reflect.DeepEqual(sitePlan, prepared) {
			t.Fatal("site preparation is not idempotent")
		}
		sitePlan = prepared
	}

	unready, err := secrets.Get(ctx, "worker-tunnel-peers", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(unready.Data[tunnel.SiteReceiveReadyKey(newPeer.PublicKey().String())]) != 0 {
		t.Fatal("site published transport readiness without a handshake")
	}
	siteDevice, err := devices.Device(sitePlan.Interface)
	if err != nil {
		t.Fatal(err)
	}
	if siteDevice.PrivateKey != newPeer || len(siteDevice.Peers) != 1 || siteDevice.Peers[0].PublicKey.String() != sitePlan.PublicKey {
		t.Fatal("site kernel device differs from validated cloud binding")
	}
	live, err = devices.Device(cfg.iface)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.Peers) != 1 || live.Peers[0].PublicKey != oldPeer.PublicKey() {
		t.Fatal("site preparation changed the legacy device")
	}
	afterRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	afterRules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(routes, afterRoutes) || !reflect.DeepEqual(rules, afterRules) {
		t.Fatal("preparation changed active routes or rules")
	}
	// Prove real resourceVersion enforcement, rather than fake-client behavior.
	stale := current.DeepCopy()
	patchData(map[string][]byte{"test-revision": []byte("new")})
	stalePatch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": stale.UID, "resourceVersion": stale.ResourceVersion}, "data": map[string][]byte{tunnel.ReceiveReadyKey: []byte("invalid")}})
	if _, err := secrets.Patch(ctx, "worker-tunnel-peers", types.MergePatchType, stalePatch, metav1.PatchOptions{}); !apierrors.IsConflict(err) {
		t.Fatalf("stale readiness write was not rejected: %v", err)
	}
	if err := reconcileReceivePreparation(ctx, worker, stale, "cp", root, func(req tunnel.ReceivePrepare, previous tunnel.PeerListDoc) ([]receiveBinding, error) {
		return prepareReceivePaths(devices, req.MeshUID, root, previous, req.Desired, req.Native, 1400, 518, req.Ports)
	}); err == nil {
		t.Fatal("stale preparation snapshot published readiness")
	}
	current, err = secrets.Get(ctx, "worker-tunnel-peers", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(current.Data[tunnel.ReceiveReadyKey]) != string(stale.Data[tunnel.ReceiveReadyKey]) {
		t.Fatal("stale write changed readiness")
	}
	patchData(map[string][]byte{tunnel.ReceivePrepareKey: []byte(`{"version":1,"unknown":true}`)})
	if err := reconcile(ctx, worker, devices, cfg); err == nil {
		t.Fatal("malformed request accepted")
	}
	patchData(map[string][]byte{tunnel.ReceivePrepareKey: requestRaw})
	if err := reconcile(ctx, worker, devices, cfg); err != nil {
		t.Fatal(err)
	}
	current, err = secrets.Get(ctx, "worker-tunnel-peers", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(current.Data[tunnel.ReceiveReadyKey]) != string(stale.Data[tunnel.ReceiveReadyKey]) {
		t.Fatal("recovery changed prepared identities")
	}
	cacheBefore, err := os.ReadFile(receiveCachePath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	changedPorts := request
	changedPorts.Ports = map[string]int{}
	for key, port := range request.Ports {
		changedPorts.Ports[key] = port + 10
	}
	changedRaw, _ := json.Marshal(changedPorts)
	patchData(map[string][]byte{tunnel.ReceivePrepareKey: changedRaw})
	if err := reconcile(ctx, worker, devices, cfg); err == nil {
		t.Fatal("live receiver moved ports without retirement")
	}
	cacheAfter, err := os.ReadFile(receiveCachePath(cfg))
	if err != nil || string(cacheBefore) != string(cacheAfter) {
		t.Fatal("rejected native preparation replaced offline recovery state", err)
	}
	patchData(map[string][]byte{tunnel.ReceivePrepareKey: requestRaw})
	if err := reconcile(ctx, worker, devices, cfg); err != nil {
		t.Fatal(err)
	}
	current, err = secrets.Get(ctx, "worker-tunnel-peers", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct prepared devices through the actual file-only bootstrap path.
	// API readiness remains tied to its original boot; offline reconstruction
	// must not publish a fresh receipt or select these devices for forwarding.
	for _, binding := range receipt.Bindings {
		link, err := netlink.LinkByName(binding.Interface)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkDel(link); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(claimPath(cfg)); err != nil {
		t.Fatal(err)
	}
	offline := cfg
	offline.peersSecretNamespace, offline.peersSecretName = "", ""
	if err := reconcile(ctx, nil, devices, offline); err != nil {
		t.Fatalf("API-free bootstrap could not restore receivers: %v", err)
	}
	unavailable := rest.CopyConfig(apiConfig)
	unavailable.Dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("test API is unavailable")
	}
	disconnected, err := kubernetes.NewForConfig(unavailable)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcile(ctx, disconnected, devices, cfg); err != nil {
		t.Fatalf("adopting dialer could not hold receivers during API outage: %v", err)
	}
	siteLink, err := netlink.LinkByName(sitePlan.Interface)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkDel(siteLink); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(ctx, disconnected, devices, siteCfg); err == nil || !strings.Contains(err.Error(), "getting secret") {
		t.Fatal("site recovery must still report unavailable forwarding configuration", err)
	}
	siteRecovered, err := devices.Device(sitePlan.Interface)
	if err != nil || siteRecovered.PrivateKey != newPeer || len(siteRecovered.Peers) != 1 || siteRecovered.Peers[0].PublicKey.String() != sitePlan.PublicKey || siteRecovered.Peers[0].Endpoint.String() != sitePlan.Endpoint {
		t.Fatal("site receiver was not recovered with its reserved identity", err)
	}
	for _, binding := range receipt.Bindings {
		device, err := devices.Device(binding.Interface)
		if err != nil || device.PublicKey.String() != binding.PublicKey || device.ListenPort != binding.ListenPort || len(device.Peers) != 1 || device.Peers[0].PublicKey.String() != binding.PeerPublicKey {
			t.Fatal("offline reconstruction changed reserved receiver identities", err)
		}
	}
	recovered, err := secrets.Get(ctx, "worker-tunnel-peers", metav1.GetOptions{})
	if err != nil || recovered.ResourceVersion != current.ResourceVersion {
		t.Fatal("offline reconstruction changed API readiness", err)
	}
	afterRoutes, err = netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	afterRules, err = netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(routes, afterRoutes) || !reflect.DeepEqual(rules, afterRules) {
		t.Fatal("offline preparation selected receiver routes or changed policy rules")
	}
	// Observe an explicit withdrawal without changing the active peer list.
	patchData(map[string][]byte{tunnel.ReceivePrepareKey: nil})
	if err := reconcileDurableSiteReceivers(ctx, worker, devices, siteCfg, mesh, newPeer); err != nil {
		t.Fatal(err)
	}
	siteLink, err = netlink.LinkByName(sitePlan.Interface)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkDel(siteLink); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(ctx, disconnected, devices, siteCfg); err == nil || !strings.Contains(err.Error(), "getting secret") {
		t.Fatal(err)
	}
	if _, err := netlink.LinkByName(sitePlan.Interface); !isLinkNotFound(err) {
		t.Fatal("withdrawn site preparation resurrected", err)
	}
	if err := reconcile(ctx, worker, devices, cfg); err != nil {
		t.Fatal(err)
	}
	for _, binding := range receipt.Bindings {
		link, err := netlink.LinkByName(binding.Interface)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkDel(link); err != nil {
			t.Fatal(err)
		}
	}
	if err := reconcile(ctx, disconnected, devices, cfg); err != nil {
		t.Fatal(err)
	}
	for _, binding := range receipt.Bindings {
		if _, err := netlink.LinkByName(binding.Interface); !isLinkNotFound(err) {
			t.Fatal("API outage resurrected withdrawn receiver preparation", err)
		}
	}
	// Exercise endpoint forwarding recovery after removing the entire legacy
	// device. Its link-owned routes disappear with it, as after a reboot.
	cloudLink, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkDel(cloudLink); err != nil {
		t.Fatal(err)
	}
	siteCfg.routeTable = 519
	mesh, err = secrets.Get(ctx, "mesh", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	meshPatch, _ = json.Marshal(map[string]any{"metadata": map[string]any{"uid": mesh.UID, "resourceVersion": mesh.ResourceVersion}, "data": map[string][]byte{tunnel.NodeTunnelAddressPrefix + "cp": []byte("10.88.0.4/32")}})
	mesh, err = secrets.Patch(ctx, "mesh", types.MergePatchType, meshPatch, metav1.PatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcile(ctx, worker, devices, siteCfg); err != nil {
		t.Fatal("initial site endpoint reconciliation", err)
	}
	beforeSite, err := devices.Device(siteCfg.iface)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeSite.Peers) != 1 || beforeSite.Peers[0].PublicKey != root.PublicKey() {
		t.Fatal("site peer was not configured")
	}
	siteLink, err = netlink.LinkByName(siteCfg.iface)
	if err != nil {
		t.Fatal(err)
	}
	beforeSiteRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: siteCfg.routeTable}, netlink.RT_FILTER_TABLE)
	if err != nil || len(beforeSiteRoutes) != 1 || beforeSiteRoutes[0].Dst.String() != "10.88.0.3/32" {
		t.Fatal("site route missing", err)
	}
	beforeSiteAddresses, err := netlink.AddrList(siteLink, netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	beforeSiteRules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	mesh, err = secrets.Get(ctx, "mesh", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkDel(siteLink); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(ctx, disconnected, devices, siteCfg); err != nil {
		t.Fatal("offline site forwarding reconstruction", err)
	}
	afterSite, err := devices.Device(siteCfg.iface)
	if err != nil || afterSite.PrivateKey != beforeSite.PrivateKey || afterSite.FirewallMark != beforeSite.FirewallMark || len(afterSite.Peers) != 1 || afterSite.Peers[0].PublicKey != beforeSite.Peers[0].PublicKey || !reflect.DeepEqual(afterSite.Peers[0].AllowedIPs, beforeSite.Peers[0].AllowedIPs) {
		t.Fatal("site forwarding identity or permissions changed", err)
	}
	siteLink, err = netlink.LinkByName(siteCfg.iface)
	if err != nil {
		t.Fatal(err)
	}
	afterSiteRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: siteCfg.routeTable}, netlink.RT_FILTER_TABLE)
	if err != nil || len(afterSiteRoutes) != 1 || afterSiteRoutes[0].LinkIndex != siteLink.Attrs().Index {
		t.Fatal("site route not reconstructed", err)
	}
	beforeSiteRoutes[0].LinkIndex = siteLink.Attrs().Index
	if !reflect.DeepEqual(beforeSiteRoutes, afterSiteRoutes) {
		t.Fatal("site route properties changed")
	}
	afterSiteAddresses, err := netlink.AddrList(siteLink, netlink.FAMILY_ALL)
	if err != nil || len(afterSiteAddresses) != len(beforeSiteAddresses) {
		t.Fatal("site addresses missing", err)
	}
	for i := range beforeSiteAddresses {
		if beforeSiteAddresses[i].IPNet.String() != afterSiteAddresses[i].IPNet.String() {
			t.Fatal("site address changed")
		}
	}
	afterSiteRules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil || !reflect.DeepEqual(beforeSiteRules, afterSiteRules) {
		t.Fatal("site policy rules changed", err)
	}
	meshAfter, err := secrets.Get(ctx, "mesh", metav1.GetOptions{})
	if err != nil || meshAfter.ResourceVersion != mesh.ResourceVersion {
		t.Fatal("offline forwarding published fresh acknowledgement", err)
	}
	// A later authoritative empty peer set must replace recovery state too.
	meshPatch, _ = json.Marshal(map[string]any{"metadata": map[string]any{"uid": meshAfter.UID, "resourceVersion": meshAfter.ResourceVersion}, "data": map[string][]byte{tunnel.PeerPublicKeyPrefix + "worker": nil, tunnel.PeerEndpointPrefix + "worker": nil, tunnel.PeerAllowedIPsPrefix + "worker": nil, tunnel.PeerRouteHostsPrefix + "worker": nil}})
	if _, err := secrets.Patch(ctx, "mesh", types.MergePatchType, meshPatch, metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(ctx, worker, devices, siteCfg); err != nil {
		t.Fatal("site withdrawal", err)
	}
	if err := netlink.LinkDel(siteLink); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(ctx, disconnected, devices, siteCfg); err != nil {
		t.Fatal("empty offline site recovery", err)
	}
	afterSite, err = devices.Device(siteCfg.iface)
	if err != nil || len(afterSite.Peers) != 0 {
		t.Fatal("site recovery resurrected withdrawn peers", err)
	}
	afterSiteRoutes, err = netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: siteCfg.routeTable}, netlink.RT_FILTER_TABLE)
	if err != nil || len(afterSiteRoutes) != 0 {
		t.Fatal("site recovery resurrected withdrawn routes", err)
	}
	retireUnpublishedEndpoint(siteCfg)
	if err := reconcile(ctx, disconnected, devices, siteCfg); err == nil || !strings.Contains(err.Error(), "getting secret") {
		t.Fatal("retired endpoint retained offline forwarding", err)
	}
	if _, err := netlink.LinkByName(siteCfg.iface); !isLinkNotFound(err) {
		t.Fatal("retired endpoint resurrected", err)
	}
	// A transit-only node keeps its observed relay and source addresses across
	// route loss and process exit, without recreating a tunnel endpoint.
	lan := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "site-recovery"}}
	if err := netlink.LinkAdd(lan); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(lan); err != nil {
		t.Fatal(err)
	}
	var gateways, sources []string
	families := map[bool]bool{}
	for _, address := range node.Status.Addresses {
		if address.Type != "InternalIP" {
			continue
		}
		ip := net.ParseIP(address.Address)
		if ip == nil {
			t.Fatal("invalid node fixture address")
		}
		bits := 64
		if ip.To4() != nil {
			ip = ip.To4()
			bits = 24
		}
		families[ip.To4() != nil] = true
		sources = append(sources, ip.String())
		addr, err := netlink.ParseAddr(fmt.Sprintf("%s/%d", ip, bits))
		if err != nil {
			t.Fatal(err)
		}
		addr.Flags = 2 // IFA_F_NODAD
		if err := netlink.AddrAdd(lan, addr); err != nil {
			t.Fatal(err)
		}
		gateway := append(net.IP(nil), ip...)
		gateway[len(gateway)-1] ^= 64
		gateways = append(gateways, gateway.String())
	}
	if len(families) != 2 {
		t.Fatal("requires dual-stack site identity")
	}
	mesh, err = secrets.Get(ctx, "mesh", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	meshPatch, _ = json.Marshal(map[string]any{"metadata": map[string]any{"uid": mesh.UID, "resourceVersion": mesh.ResourceVersion}, "data": map[string][]byte{
		tunnel.NodeTunnelAddressPrefix + "cp":    nil,
		tunnel.SiteAddressesPrefix + "cp":        []byte(strings.Join(sources, ",")),
		tunnel.NodePublicKeyPrefix + "relay":     []byte(oldPeer.PublicKey().String()),
		tunnel.NodeTunnelAddressPrefix + "relay": []byte("10.88.0.5/32"),
		tunnel.NodeAddressesPrefix + "relay":     []byte(strings.Join(gateways, ",")),
		tunnel.PeerPublicKeyPrefix + "worker":    []byte(root.PublicKey().String()),
		tunnel.PeerEndpointPrefix + "worker":     []byte("198.51.100.10:51820"),
		tunnel.PeerAllowedIPsPrefix + "worker":   []byte("10.88.0.3/32,fd88::3/128"),
		tunnel.PeerRouteHostsPrefix + "worker":   []byte("10.88.0.3,fd88::3"),
	}})
	if _, err := secrets.Patch(ctx, "mesh", types.MergePatchType, meshPatch, metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(ctx, worker, devices, siteCfg); err != nil {
		t.Fatal("initial transit", err)
	}
	transitRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: siteCfg.routeTable}, netlink.RT_FILTER_TABLE)
	if err != nil || len(transitRoutes) != 2 {
		t.Fatal("dual-stack transit routes missing", err)
	}
	for _, route := range transitRoutes {
		if route.Src == nil || route.Gw == nil || route.LinkIndex != lan.Attrs().Index {
			t.Fatal("transit lacks source or gateway")
		}
	}
	mesh, err = secrets.Get(ctx, "mesh", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	transitRules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"0.0.0.0/0", "::/0", "invalid"} {
		if err := installSiteTransit(siteCfg, &tunnel.TransitSpec{Via: gateways[0], Blocks: []string{invalid}}, sources); err == nil {
			t.Fatal("unsafe transit input accepted")
		}
		current, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: siteCfg.routeTable}, netlink.RT_FILTER_TABLE)
		if err != nil || !reflect.DeepEqual(transitRoutes, current) {
			t.Fatal("rejected transit changed routes", err)
		}
		currentRules, err := netlink.RuleList(netlink.FAMILY_ALL)
		if err != nil || !reflect.DeepEqual(transitRules, currentRules) {
			t.Fatal("rejected transit changed policy", err)
		}
	}
	retireUnpublishedEndpoint(siteCfg)
	for i := range transitRoutes {
		if err := netlink.RouteDel(&transitRoutes[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := reconcile(ctx, disconnected, devices, siteCfg); err != nil {
		t.Fatal("offline transit reconstruction", err)
	}
	restoredTransit, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: siteCfg.routeTable}, netlink.RT_FILTER_TABLE)
	if err != nil || !reflect.DeepEqual(transitRoutes, restoredTransit) {
		t.Fatal("offline transit changed source or relay", err)
	}
	if _, err := netlink.LinkByName(siteCfg.iface); !isLinkNotFound(err) {
		t.Fatal("transit recovery created an endpoint", err)
	}
	meshAfter, err = secrets.Get(ctx, "mesh", metav1.GetOptions{})
	if err != nil || meshAfter.ResourceVersion != mesh.ResourceVersion {
		t.Fatal("offline transit published acknowledgement", err)
	}
	checkNativeBGPRecovery(t, ctx, worker, disconnected, devices, siteCfg, sources)
	t.Log("Real API receiver preparation, offline site forwarding reconstruction, and authoritative withdrawal recovery passed")
}
