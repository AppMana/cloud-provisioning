package main

import (
	"context"
	"encoding/json"
	"os"
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
	update([]tunnel.PeerSpec{})
	check(0)
	nativeSiteAPI(t, cfg, devices, bootstrap, admin, worker, ns, subject)
	t.Logf("Real API: ServiceAccount=%v, restricted Secret access, cache repair, re-addition, withdrawal and applied-hash acknowledgements passed", serviceAccount)
}
