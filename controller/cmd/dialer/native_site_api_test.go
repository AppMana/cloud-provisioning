package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func nativeSiteAPI(t *testing.T, cfg config, devices *wgctrl.Client, bootstrap tunnel.PeersFileDoc, admin, worker *kubernetes.Clientset, ns string, subject rbacv1.Subject) {
	t.Helper()
	ctx := context.Background()
	name := "site-canary"
	role, err := admin.RbacV1().Roles(ns).Get(ctx, "worker", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	role.Rules[0].ResourceNames = append(role.Rules[0].ResourceNames, "mesh")
	if _, err = admin.RbacV1().Roles(ns).Update(ctx, role, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "site-node-reader"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list"}}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "site-node-reader"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "site-node-reader"}, Subjects: []rbacv1.Subject{subject}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	node, err := admin.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	node, err = admin.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	key, err := wgtypes.ParseKey(bootstrap.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg.privateKeyFile = filepath.Join(t.TempDir(), "private-key")
	if err = os.WriteFile(cfg.privateKeyFile, []byte(bootstrap.PrivateKey), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.peersFile = ""
	cfg.peersSecretNamespace = ""
	cfg.peersSecretName = ""
	cfg.secretNamespace = ns
	cfg.secretName = "mesh"
	cfg.nodeName = name
	secrets := admin.CoreV1().Secrets(ns)
	_, err = secrets.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh"}, Data: map[string][]byte{tunnel.NodePublicKeyPrefix + name: []byte(key.PublicKey().String()), tunnel.NodeTunnelAddressPrefix + name: []byte(bootstrap.LocalAddress)}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	update := func(present bool) {
		t.Helper()
		s, err := secrets.Get(ctx, "mesh", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		entries := map[string]string{tunnel.PeerPublicKeyPrefix + "worker": bootstrap.Peers[0].PublicKey, tunnel.PeerEndpointPrefix + "worker": bootstrap.Peers[0].Endpoint, tunnel.PeerAllowedIPsPrefix + "worker": "10.253.253.2/32", tunnel.PeerRouteHostsPrefix + "worker": "10.253.253.2"}
		for k, v := range entries {
			if present {
				s.Data[k] = []byte(v)
			} else {
				delete(s.Data, k)
			}
		}
		if _, err = secrets.Update(ctx, s, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(expected int) {
		t.Helper()
		if err := reconcile(ctx, worker, devices, cfg); err != nil {
			t.Fatal(err)
		}
		s, err := secrets.Get(ctx, "mesh", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !tunnel.SiteConverged(s.Data, name, string(node.UID)) {
			t.Fatal("site receipt does not qualify current API Node identity and mesh")
		}
		d, err := devices.Device(cfg.iface)
		if err != nil || len(d.Peers) != expected || d.PrivateKey != key {
			t.Fatal("native site peer or identity mismatch")
		}
		link, err := netlink.LinkByName(cfg.iface)
		if err != nil {
			t.Fatal(err)
		}
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable, LinkIndex: link.Attrs().Index}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
		if err != nil || len(routes) != expected {
			t.Fatal("native site routes differ from worker membership")
		}
	}
	update(true)
	check(1)
	update(false)
	check(0)
	update(true)
	check(1)
	update(false)
	check(0)
	oldUID := node.UID
	if err = admin.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	node, err = admin.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil || node.UID == oldUID {
		t.Fatal("Node recreation failed")
	}
	s, err := secrets.Get(ctx, "mesh", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tunnel.SiteConverged(s.Data, name, string(node.UID)) {
		t.Fatal("old site receipt qualified replacement Node")
	}
	check(0)
	t.Log("Real API site: add/remove/re-add/remove, native routes, Node-bound receipts and rejection of stale receipt after Node recreation passed")
}
