package main

import (
	"context"
	"encoding/json"
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
	cfg := config{iface: "cldtapibase", peersFile: filepath.Join(t.TempDir(), "identity.json"), peersSecretNamespace: namespace, peersSecretName: "adoption", nodeName: "cp", mtu: 1400, routeTable: 518, fwmark: 518, pollInterval: time.Second}
	bootstrap := tunnel.PeersFileDoc{PrivateKey: root.String(), LocalAddress: "10.88.0.3/32", Peers: base.Peers}
	raw, _ := json.Marshal(bootstrap)
	if err := os.WriteFile(cfg.peersFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	patchData := func(data map[string][]byte) {
		t.Helper()
		current, err := secrets.Get(ctx, "adoption", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": current.UID, "resourceVersion": current.ResourceVersion}, "data": data})
		if _, err := secrets.Patch(ctx, "adoption", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
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
	before, err := secrets.Get(ctx, "adoption", metav1.GetOptions{})
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
	request := tunnel.ReceivePrepare{Version: 1, MeshUID: string(before.UID), NodeName: "cp", NodeUID: nodeUID, BootID: node.Status.NodeInfo.BootID, RootPublicKey: root.PublicKey().String(), BaseHash: tunnel.HashPeerList(baseRaw), Desired: tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: newPeer.PublicKey().String(), WGAllowedIPs: []string{"10.90.0.10/32"}}}}, Native: []netip.Prefix{netip.MustParsePrefix("172.29.0.0/24")}}
	requestRaw, _ := json.Marshal(request)
	patchData(map[string][]byte{tunnel.ReceivePrepareKey: requestRaw})
	if err := reconcile(ctx, worker, devices, cfg); err != nil {
		t.Fatal(err)
	}
	current, err := secrets.Get(ctx, "adoption", metav1.GetOptions{})
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
	if _, err := secrets.Patch(ctx, "adoption", types.MergePatchType, stalePatch, metav1.PatchOptions{}); !apierrors.IsConflict(err) {
		t.Fatalf("stale readiness write was not rejected: %v", err)
	}
	if err := reconcileReceivePreparation(ctx, worker, stale, "cp", root, func(req tunnel.ReceivePrepare, previous tunnel.PeerListDoc) ([]receiveBinding, error) {
		return prepareReceivePaths(devices, req.MeshUID, root, previous, req.Desired, req.Native, 1400, 518)
	}); err == nil {
		t.Fatal("stale preparation snapshot published readiness")
	}
	current, err = secrets.Get(ctx, "adoption", metav1.GetOptions{})
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
	current, err = secrets.Get(ctx, "adoption", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(current.Data[tunnel.ReceiveReadyKey]) != string(stale.Data[tunnel.ReceiveReadyKey]) {
		t.Fatal("recovery changed prepared identities")
	}
	t.Log("Real API: scoped access, reconciler preparation, unchanged forwarding, stale-write rejection, and malformed-request recovery passed")
}
