package main

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Synthetic API state with real isolated routes and BGP. This tests the entire
// ordinary install/unpublish/acknowledgement path, not provider deletion, real
// authentication, external convergence, packet continuity or version rollback.
func verifyNativeOrdinaryFollower(t *testing.T, api kubernetes.Interface, cfg config, key wgtypes.Key, lockPath string, speaker *transitSpeaker, admitted receiveObserverAdmission, plan tunnel.ReceiveTransitionPlan) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	own := []string{"10.90.0.4", "fd90::4"}
	mesh, err := api.CoreV1().Secrets(plan.Namespace).Create(ctx, admitted.Mesh.DeepCopy(), metav1.CreateOptions{})
	must(err)
	_, err = api.CoreV1().Nodes().Create(ctx, admitted.Node.DeepCopy(), metav1.CreateOptions{})
	must(err)
	oldRoot := string(mesh.Data[tunnel.PeerPublicKeyPrefix+"survivor"])
	doc, err := json.Marshal(plan.Desired)
	must(err)
	adoption, err := api.CoreV1().Secrets(plan.Namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName("survivor"), Namespace: plan.Namespace, UID: "ordinary-adoption", ResourceVersion: "1"}, Data: map[string][]byte{tunnel.CloudPeersKey: doc}}, metav1.CreateOptions{})
	must(err)
	ordinaryRoutes := map[string]string{"172.29.0.3/32": "10.90.0.2", "fd29::3/128": "fd90::2"}
	must(withdrawReceiveDeviceRoutes(ctx, cfg, []map[string]receiveRouteSelection{{}}, own, ordinaryRoutes))
	assertOrdinary := func(present bool) {
		t.Helper()
		selected := cfg
		selected.receiveRouteScope = map[string]bool{}
		selected.receiveRelayRoutes = map[string]string{}
		hosts := []net.IPNet{}
		for raw, via := range ordinaryRoutes {
			selected.receiveRouteScope[raw] = true
			if present {
				selected.receiveRelayRoutes[raw] = via
				_, dst, _ := net.ParseCIDR(raw)
				hosts = append(hosts, *dst)
			}
		}
		must(verifyReceiveRouteInstallation(selected, hosts, nil, own))
	}
	run := func() error {
		if err := reconcileReceiveFollower(ctx, api, cfg, speaker, key, lockPath, admitted, func() error { return nil }); err != nil {
			return err
		}
		return acknowledgeReceiveFollower(ctx, api, cfg, key, lockPath, admitted, func() error { return nil })
	}
	must(run())
	assertOrdinary(false)
	current, err := api.CoreV1().Secrets(plan.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
	must(err)
	if len(current.Data[tunnel.SiteAppliedPrefix+cfg.nodeName]) != 0 {
		t.Fatal("pending ordinary adoption acknowledged forwarding")
	}
	adoption.Annotations = map[string]string{tunnel.AppliedListAnnotation: tunnel.HashPeerList(doc)}
	adoption.ResourceVersion = "2"
	_, err = api.CoreV1().Secrets(plan.Namespace).Update(ctx, adoption, metav1.UpdateOptions{})
	must(err)
	must(run())
	assertOrdinary(true)
	current, err = api.CoreV1().Secrets(plan.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
	must(err)
	if !tunnel.SiteConverged(current.Data, cfg.nodeName, string(admitted.Node.UID)) {
		t.Fatal("complete ordinary forwarding was not acknowledged")
	}
	current.Data, _, err = tunnel.WithdrawRemotePeer(current.Data, "survivor", oldRoot)
	must(err)
	current.ResourceVersion = "3"
	admitted.Mesh, err = api.CoreV1().Secrets(plan.Namespace).Update(ctx, current, metav1.UpdateOptions{})
	must(err)
	must(run())
	assertOrdinary(false)
	current, err = api.CoreV1().Secrets(plan.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
	must(err)
	if !tunnel.SiteConverged(current.Data, cfg.nodeName, string(admitted.Node.UID)) {
		t.Fatal("ordinary withdrawal was not acknowledged")
	}
	boot, err := localReceiveBootID()
	must(err)
	expected := receiveInventoryCheckpoint{Version: 1, InventoryUID: string(admitted.Metadata.UID), Inventory: admitted.Inventory, NodeName: cfg.nodeName, NodeUID: string(admitted.Node.UID), BootID: boot, PublicKey: key.PublicKey().String()}
	saved, err := readReceiveFollowerCache(lockPath+".follower.json", cfg, expected, own)
	must(err)
	if saved.Version != 2 || !saved.Retired[oldRoot] || len(saved.Routes[oldRoot]) != 0 {
		t.Fatal("ordinary withdrawal lost its completed key fence")
	}
	publish := func(name, root, version string) {
		current, err := api.CoreV1().Secrets(plan.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
		must(err)
		current.Data[tunnel.PeerPublicKeyPrefix+name] = []byte(root)
		current.Data[tunnel.PeerEndpointPrefix+name] = []byte("198.51.100.3:51820")
		current.Data[tunnel.PeerAllowedIPsPrefix+name] = []byte("172.29.0.3/32,fd29::3/128")
		current.Data[tunnel.PeerRouteHostsPrefix+name] = []byte("172.29.0.3,fd29::3")
		current.ResourceVersion = version
		admitted.Mesh, err = api.CoreV1().Secrets(plan.Namespace).Update(ctx, current, metav1.UpdateOptions{})
		must(err)
	}
	publish("survivor", oldRoot, "4")
	if err := run(); err == nil {
		t.Fatal("old worker key restored follower ownership")
	}
	assertOrdinary(false)
	current = admitted.Mesh.DeepCopy()
	current.Data, _, err = tunnel.WithdrawRemotePeer(current.Data, "survivor", oldRoot)
	must(err)
	current.ResourceVersion = "5"
	admitted.Mesh, err = api.CoreV1().Secrets(plan.Namespace).Update(ctx, current, metav1.UpdateOptions{})
	must(err)
	replacement, err := wgtypes.GeneratePrivateKey()
	must(err)
	publish("replacement", replacement.PublicKey().String(), "6")
	_, err = api.CoreV1().Secrets(plan.Namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName("replacement"), Namespace: plan.Namespace, UID: "replacement-adoption", ResourceVersion: "1", Annotations: map[string]string{tunnel.AppliedListAnnotation: tunnel.HashPeerList(doc)}}, Data: map[string][]byte{tunnel.CloudPeersKey: doc}}, metav1.CreateOptions{})
	must(err)
	must(run())
	assertOrdinary(true)
	saved, err = readReceiveFollowerCache(lockPath+".follower.json", cfg, expected, own)
	must(err)
	if !saved.Retired[oldRoot] || len(saved.Routes[oldRoot]) != 0 || len(saved.Routes[replacement.PublicKey().String()]) != 1 {
		t.Fatal("address reuse erased retired identity or failed to install new owner")
	}
	current, err = api.CoreV1().Secrets(plan.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
	must(err)
	if !tunnel.SiteConverged(current.Data, cfg.nodeName, string(admitted.Node.UID)) {
		t.Fatal("replacement did not converge")
	}
}
