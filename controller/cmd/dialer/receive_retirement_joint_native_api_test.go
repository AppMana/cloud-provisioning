package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const nativeJointReceiptImage = "test.invalid/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// An isolated real API and real kernel run the complete observation-to-release
// path. Pod/Node status, serving history and rollout compatibility are synthetic;
// this is not a live agent rollout, workload continuity or actual reboot test.
func TestNativeJointRetirementRealAPI(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_JOINT_REAL_API") != "1" {
		t.Skip("requires isolated guest namespaces and API assets")
	}
	requireJointNativeNamespace(t)
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("real API assets required")
	}
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.Attrs().Name != "lo" {
			t.Fatal("namespace must start empty")
		}
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{AttachControlPlaneOutput: true}
	// Like the existing native adoption fixture, the isolated namespace has
	// no default route from which the API server can infer an advertised IP.
	env.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "10.253.253.1")
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	api, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := api.CoreV1().Namespaces().Get(context.Background(), "kube-system", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	verifyJointRetirementExecution(t, api, string(cluster.UID), true)
}

func requireJointNativeNamespace(t *testing.T) {
	t.Helper()
	for _, kind := range []string{"net", "mnt"} {
		self, e := os.Readlink("/proc/self/ns/" + kind)
		host, h := os.Readlink("/proc/1/ns/" + kind)
		if e != nil || h != nil || self == host {
			t.Fatal("refusing guest host namespace")
		}
	}
}

func nativeJointReceiptPod(t *testing.T, api kubernetes.Interface, namespace, nodeName string) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	sa, err := api.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "agent"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"app": "native-joint-agent"}
	spec := corev1.PodSpec{ServiceAccountName: sa.Name, NodeName: nodeName, Containers: []corev1.Container{{Name: "dialer", Image: nativeJointReceiptImage}}}
	daemon, err := api.AppsV1().DaemonSets(namespace).Create(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent"}, Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: spec}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent", Labels: labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(daemon, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}}, Spec: spec}
}

func prepareNativeJointExecution(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, plans []tunnel.ReceiveTransitionPlan, published *corev1.Secret) receiveRetirementNative {
	t.Helper()
	requireJointNativeNamespace(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	must(os.WriteFile(cfg.privateKeyFile, []byte(key.String()), 0600))
	cached := map[string]bool{}
	for _, plan := range plans {
		cached[plan.MachineName] = true
	}
	must(saveSiteForwardingCache(cfg, key, published, nil, cached, nil))
	speaker, err := startTransitSpeaker(ctx, -1, 64512, "10.90.0.2,fd90::2")
	must(err)
	t.Cleanup(func() { speaker.stop(ctx) })
	must(saveTransitAdvertisements(cfg, speaker, published, []string{}))
	ads, _, err := transitAdvertisementPlan(published.Data, cfg.nodeName)
	must(err)
	must(speaker.reconcile(ctx, nil, ads))
	devices, err := wgctrl.New()
	must(err)
	t.Cleanup(func() { devices.Close() })
	legacy := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: cfg.iface}}
	must(netlink.LinkAdd(legacy))
	must(netlink.LinkSetUp(legacy))
	t.Cleanup(func() { netlink.LinkDel(legacy) })
	for _, raw := range []string{"10.90.0.2/24", "fd90::2/64"} {
		a, e := netlink.ParseAddr(raw)
		must(e)
		a.Flags = unix.IFA_F_NODAD
		must(netlink.AddrAdd(legacy, a))
	}
	must(ensureRouteRule(cfg.routeTable, cfg.fwmark))
	t.Cleanup(func() { removeRouteRule(cfg.routeTable, cfg.fwmark) })
	allowed := func(raw string) net.IPNet { _, prefix, e := net.ParseCIDR(raw); must(e); return *prefix }
	peers := []wgtypes.PeerConfig{}
	interfaces := []string{}
	for _, plan := range plans {
		root, e := wgtypes.ParseKey(plan.RootPublicKey)
		must(e)
		ips := []net.IPNet{}
		for _, raw := range plan.CloudAddresses {
			ips = append(ips, allowed(raw))
		}
		peers = append(peers, wgtypes.PeerConfig{PublicKey: root, AllowedIPs: ips})
		selected, e := receivePhaseSelections(key, plan, plan.Sites[key.PublicKey().String()].NodeUID)
		must(e)
		path := selected[0]
		receiver := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: path.Interface, Alias: path.Alias}}
		must(netlink.LinkAdd(receiver))
		must(netlink.LinkSetAlias(receiver, path.Alias))
		must(netlink.LinkSetUp(receiver))
		t.Cleanup(func() { netlink.LinkDel(receiver) })
		remote, e := wgtypes.ParseKey(path.PeerPublicKey)
		must(e)
		must(devices.ConfigureDevice(path.Interface, wgtypes.Config{PrivateKey: &key, FirewallMark: &cfg.fwmark, Peers: []wgtypes.PeerConfig{{PublicKey: remote, AllowedIPs: ips}}}))
		for _, raw := range plan.CloudAddresses {
			dst := allowed(raw)
			src := net.ParseIP("10.90.0.2")
			if dst.IP.To4() == nil {
				src = net.ParseIP("fd90::2")
			}
			must(netlink.RouteAdd(&netlink.Route{LinkIndex: receiver.Attrs().Index, Dst: &dst, Src: src, Table: cfg.routeTable, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT, Priority: receiveRoutePriority(&dst)}))
		}
		interfaces = append(interfaces, path.Interface)
	}
	must(devices.ConfigureDevice(cfg.iface, wgtypes.Config{PrivateKey: &key, FirewallMark: &cfg.fwmark, Peers: peers}))
	control := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "jointcontrol"}}
	must(netlink.LinkAdd(control))
	must(netlink.LinkSetUp(control))
	t.Cleanup(func() { netlink.LinkDel(control) })
	controlDst := allowed("198.19.1.0/24")
	must(netlink.RouteAdd(&netlink.Route{LinkIndex: control.Attrs().Index, Dst: &controlDst, Table: 777, Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_STATIC}))
	t.Cleanup(func() {
		for _, name := range interfaces {
			if _, err := netlink.LinkByName(name); !isLinkNotFound(err) {
				t.Error("retired native receiver remains", name, err)
			}
		}
		routes, e := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
		if e != nil || len(routes) != 0 {
			t.Error("retired native routes remain", e, routes)
		}
		unchanged, e := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: 777}, netlink.RT_FILTER_TABLE)
		if e != nil || len(unchanged) != 1 || unchanged[0].LinkIndex != control.Attrs().Index || unchanged[0].Dst == nil || unchanged[0].Dst.String() != controlDst.String() || unchanged[0].Protocol != unix.RTPROT_STATIC {
			t.Error("unrelated native route changed", e)
		}
		device, e := devices.Device(cfg.iface)
		if e != nil || device.PublicKey != key.PublicKey() || len(device.Peers) != 0 {
			t.Error("legacy identity changed or retired peer remains", e)
		}
		for _, plan := range plans {
			for _, raw := range plan.CloudAddresses {
				if _, e := advertisedMED(ctx, speaker, raw); e == nil {
					t.Error("retired local BGP advertisement remains", raw)
				}
			}
		}
		_, saved, e := readTransitAdvertisements(cfg, speaker)
		if e != nil {
			t.Error(e)
		} else {
			for _, plan := range plans {
				if len(saved.Data[tunnel.PeerPublicKeyPrefix+plan.MachineName]) != 0 {
					t.Error("retired executable cache remains")
				}
			}
		}
		if e := verifyReceivePolicyRules(cfg); e != nil {
			t.Error(e)
		}
	})
	return func(ctx context.Context, path string, admitted admittedReceiveRetirement) error {
		memberStore, memberScope := retirementMemberScope(store, scope, admitted, admitted.Withdrawal.MachineUID)
		return retireAdmittedSiteReceiversLocked(ctx, api, devices, speaker, memberStore, memberScope, cfg, key, path, admitted)
	}
}

func verifyNativeJointReceiptLifecycle(t *testing.T, api kubernetes.Interface, stores []handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, pod *corev1.Pod, native receiveRetirementNative) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	sa, err := api.CoreV1().ServiceAccounts(scope.Namespace).Get(ctx, pod.Spec.ServiceAccountName, metav1.GetOptions{})
	must(err)
	daemon, err := api.AppsV1().DaemonSets(scope.Namespace).Get(ctx, "agent", metav1.GetOptions{})
	must(err)
	audience := "native-joint-receipts"
	token, err := api.CoreV1().ServiceAccounts(scope.Namespace).CreateToken(ctx, sa.Name, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: []string{audience}, BoundObjectRef: &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}}}, metav1.CreateOptions{})
	must(err)
	policy := handover.PodReceiptAuthenticator{API: api, Audience: audience, Namespace: scope.Namespace, ServiceAccountName: sa.Name, ServiceAccountUID: string(sa.UID), DaemonSetName: daemon.Name, DaemonSetUID: string(daemon.UID), ContainerName: "dialer", Image: nativeJointReceiptImage, ImageID: nativeJointReceiptImage}
	// Node absence is read from the real API. Serving history and compatible
	// rollback policy are fixture assumptions, not production release approval.
	checkOwner := func(source attachment.ReceiveRetirementSet) error {
		plan, err := tunnel.DecodeReceiveTransition(source.Journals[source.Withdrawal.MachineUID], source.Inventory.ClusterUID)
		if err != nil {
			return err
		}
		if _, err := api.CoreV1().Nodes().Get(ctx, plan.CloudNodeName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("departing Node absence not established")
		}
		if _, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, plan.AdoptionName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("departing adoption absence not established")
		}
		return nil
	}
	observe := func(store handover.Store) (attachment.ReceiveRetirementSet, []handover.PodReceipt) {
		snapshot, err := store.Load(ctx)
		must(err)
		plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
		must(err)
		memberScope := scope
		memberScope.MachineUID = plan.MachineUID
		entries, err := executeReceiveRetirement(ctx, api, store, memberScope, cfg, key, "containerd://joint", native)
		must(err)
		if len(entries) != 1 {
			t.Fatal("native cleanup did not generate an observation")
		}
		if entries[0].BearerToken != "" {
			t.Fatal("executor leaked credentials into native evidence")
		}
		entries[0].BearerToken = token.Status.Token
		source, err := attachment.ReadReceiveRetirementSet(ctx, api, store, scope.Namespace, scope.MeshName, scope.MeshUID, plan.MachineUID)
		must(err)
		return source, entries
	}
	for _, store := range stores {
		source, entries := observe(store)
		policies := map[string]handover.PodReceiptAuthenticator{entries[0].Receipt.NodeUID: policy}
		collection, err := source.ReceiptCollectionWithFinalization(ctx, api, store, policies, 30*time.Second, func() error { return checkOwner(source) })
		must(err)
		if collection.Digest != entries[0].Receipt.RoundHash {
			t.Fatal("native and controller receipt bindings differ")
		}
		must(collection.Observe(ctx, entries))
		must(collection.Complete(ctx, entries))
	}
	cfg.observeFinalizedRetirements = true
	source, entries := observe(stores[0])
	policies := map[string]handover.PodReceiptAuthenticator{entries[0].Receipt.NodeUID: policy}
	collection, err := source.ReceiptCollectionWithCohortRelease(ctx, api, stores[0], policies, 30*time.Second, checkOwner)
	must(err)
	must(collection.Observe(ctx, entries))
	must(collection.Complete(ctx, entries))
	inventory, _, err := tunnel.ReadReceiveInventory(ctx, stores[0].ConfigMaps, stores[0].ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	must(err)
	if len(inventory.MachineUIDs) != 0 || len(inventory.Publications) != 0 || len(inventory.Released) != len(stores) {
		t.Fatal("native evidence did not release the whole group")
	}
	for _, store := range stores {
		uid := ""
		for member, pub := range inventory.Released {
			if pub.JournalUID == store.ExpectedUID {
				uid = member
			}
		}
		if uid == "" {
			t.Fatal("retired journal missing")
		}
		_, ok, err := attachment.ReadReleasedReceiveJournal(ctx, api, store, scope.Namespace, scope.MeshName, scope.MeshUID, uid, string(source.Metadata.UID), 30*time.Second)
		must(err)
		if !ok {
			t.Fatal("native release lost retained history")
		}
		if err := tunnel.ReserveReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, source.Mesh, uid, nil); err == nil {
			t.Fatal("retired owner reclaimed its reservation")
		}
	}
	if err := collection.Complete(ctx, entries); err == nil {
		t.Fatal("old authenticated collection released twice")
	}
}
