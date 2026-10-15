package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func nativeAdoptionAPI(t *testing.T, cfg config, devices *wgctrl.Client, bootstrap tunnel.PeersFileDoc) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("isolated API binaries required inside VM")
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err = netlink.LinkSetUp(lo); err != nil {
		t.Fatal(err)
	}
	api := &envtest.Environment{AttachControlPlaneOutput: true}
	api.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC").Set("advertise-address", "10.253.253.1").Set("service-account-issuer", "https://cldt-native-api").Set("api-audiences", "https://cldt-native-api")
	adminConfig, err := api.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := api.Stop(); err != nil {
			t.Error(err)
		}
	})
	admin, err := kubernetes.NewForConfig(adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := "native-adoption"
	if _, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	secrets := admin.CoreV1().Secrets(ns)
	if _, err = secrets.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "adoption"}, Data: map[string][]byte{tunnel.CloudPeersKey: []byte(`{"peers":[]}`)}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = secrets.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.RbacV1().Roles(ns).Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"adoption"}, Verbs: []string{"get", "patch"}}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	serviceAccount := os.Getenv("CLDT_NATIVE_SERVICEACCOUNT") == "1"
	subject := rbacv1.Subject{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "canary-worker"}
	if serviceAccount {
		if _, err = admin.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "canary-worker"}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		subject = rbacv1.Subject{Kind: "ServiceAccount", Namespace: ns, Name: "canary-worker"}
	}
	if _, err = admin.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "worker"}, Subjects: []rbacv1.Subject{subject}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var worker *kubernetes.Clientset
	if serviceAccount {
		seconds := int64(600)
		token, tokenErr := admin.CoreV1().ServiceAccounts(ns).CreateToken(ctx, "canary-worker", &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: []string{"https://cldt-native-api"}, ExpirationSeconds: &seconds}}, metav1.CreateOptions{})
		if tokenErr != nil {
			t.Fatal(tokenErr)
		}
		if token.Status.Token == "" {
			t.Fatal("empty ServiceAccount token")
		}
		// Retain only API trust, never the setup client's certificate identity.
		worker, err = kubernetes.NewForConfig(&rest.Config{Host: adminConfig.Host, TLSClientConfig: rest.TLSClientConfig{CAData: adminConfig.CAData, CAFile: adminConfig.CAFile}, BearerToken: token.Status.Token})
	} else {
		user, userErr := api.AddUser(envtest.User{Name: "canary-worker"}, nil)
		if userErr != nil {
			t.Fatal(userErr)
		}
		worker, err = kubernetes.NewForConfig(user.Config())
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.CoreV1().Secrets(ns).Get(ctx, "unrelated", metav1.GetOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("unrelated Secret was not forbidden: %v", err)
	}
	if err = os.WriteFile(cachePath(cfg), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.peersSecretNamespace = ns
	cfg.peersSecretName = "adoption"
	check := func(expected int) {
		t.Helper()
		if err := reconcile(ctx, worker, devices, cfg); err != nil {
			t.Fatal(err)
		}
		current, err := secrets.Get(ctx, "adoption", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if current.Annotations[tunnel.AppliedListAnnotation] != tunnel.HashPeerList(current.Data[tunnel.CloudPeersKey]) {
			t.Fatal("actual API acknowledgement mismatch")
		}
		device, err := devices.Device(cfg.iface)
		if err != nil {
			t.Fatal(err)
		}
		key, err := wgtypes.ParseKey(bootstrap.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		if len(device.Peers) != expected || device.PrivateKey != key {
			t.Fatal("native membership or private identity mismatch")
		}
		cached, err := readCachedPeers(cachePath(cfg))
		if err != nil || len(cached.Peers) != expected {
			t.Fatal("durable state mismatch")
		}
	}
	check(0)
	update := func(peers []tunnel.PeerSpec) {
		t.Helper()
		current, err := secrets.Get(ctx, "adoption", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(tunnel.PeerListDoc{Peers: peers})
		if err != nil {
			t.Fatal(err)
		}
		current.Data[tunnel.CloudPeersKey] = raw
		if _, err = secrets.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	update(bootstrap.Peers)
	check(1)
	cacheBefore, err := os.ReadFile(cachePath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	invalid := append([]tunnel.PeerSpec(nil), bootstrap.Peers...)
	invalid[0].PublicKey = "invalid-key"
	update(invalid)
	if err := reconcile(ctx, worker, devices, cfg); err == nil || !strings.Contains(err.Error(), "parsing peer public key") {
		t.Fatalf("invalid peer update was not rejected: %v", err)
	}
	cacheAfter, err := os.ReadFile(cachePath(cfg))
	if err != nil || !bytes.Equal(cacheBefore, cacheAfter) {
		t.Fatal("rejected update replaced durable recovery state")
	}
	invalidSecret, err := secrets.Get(ctx, "adoption", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if invalidSecret.Annotations[tunnel.AppliedListAnnotation] == tunnel.HashPeerList(invalidSecret.Data[tunnel.CloudPeersKey]) {
		t.Fatal("invalid peer update was acknowledged")
	}
	// Reconstruct the kernel interface without API access, after the adopting
	// process has released its claim. Recovery must use the prior valid cache.
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkDel(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(claimPath(cfg)); err != nil {
		t.Fatal(err)
	}
	bootConfig := cfg
	bootConfig.peersSecretNamespace = ""
	bootConfig.peersSecretName = ""
	if err := reconcile(ctx, nil, devices, bootConfig); err != nil {
		t.Fatalf("offline reconstruction after a rejected update: %v", err)
	}
	restored, err := devices.Device(cfg.iface)
	if err != nil || len(restored.Peers) != 1 || restored.Peers[0].PublicKey.String() != bootstrap.Peers[0].PublicKey {
		t.Fatalf("offline reconstruction lost the previous valid peer: %v", err)
	}
	// A newer cache format must not be interpreted as legacy forwarding by an
	// older reader. Reject it before touching the surviving device or routes.
	var unsupported map[string]any
	if err := json.Unmarshal(cacheBefore, &unsupported); err != nil {
		t.Fatal(err)
	}
	unsupported["activeGeneration"] = "unsupported"
	unsupportedRaw, err := json.Marshal(unsupported)
	if err != nil {
		t.Fatal(err)
	}
	routesBefore, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath(cfg), unsupportedRaw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(ctx, nil, devices, bootConfig); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unsupported recovery format was not rejected: %v", err)
	}
	preserved, err := devices.Device(cfg.iface)
	if err != nil || preserved.PublicKey != restored.PublicKey || len(preserved.Peers) != 1 || preserved.Peers[0].PublicKey != restored.Peers[0].PublicKey || !reflect.DeepEqual(preserved.Peers[0].AllowedIPs, restored.Peers[0].AllowedIPs) {
		t.Fatal("unsupported cache changed the surviving device", err)
	}
	routesAfter, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
	if err != nil || !reflect.DeepEqual(routesBefore, routesAfter) {
		t.Fatal("unsupported cache changed surviving routes", err)
	}
	if err := os.WriteFile(cachePath(cfg), cacheBefore, 0600); err != nil {
		t.Fatal(err)
	}
	update([]tunnel.PeerSpec{})
	// A failed durable write must not withdraw the working peer or publish an
	// acknowledgement for state that cannot be recovered after an API outage.
	if err := os.Remove(cachePath(cfg)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cachePath(cfg), 0700); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(ctx, worker, devices, cfg); err == nil || !strings.Contains(err.Error(), "persisting adopted peer list") {
		t.Fatalf("cache failure did not stop adoption: %v", err)
	}
	currentDevice, err := devices.Device(cfg.iface)
	if err != nil || len(currentDevice.Peers) != 1 {
		t.Fatalf("cache failure changed active peer membership: %v", err)
	}
	currentSecret, err := secrets.Get(ctx, "adoption", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if currentSecret.Annotations[tunnel.AppliedListAnnotation] == tunnel.HashPeerList(currentSecret.Data[tunnel.CloudPeersKey]) {
		t.Fatal("cache failure acknowledged unapplied withdrawal")
	}
	if err := os.Remove(cachePath(cfg)); err != nil {
		t.Fatal(err)
	}
	check(0)
	nativeSiteAPI(t, cfg, devices, bootstrap, admin, worker, ns, subject)
	t.Logf("Real API: ServiceAccount=%v, restricted Secret access, cache repair, re-addition, withdrawal and applied-hash acknowledgements passed", serviceAccount)
}
