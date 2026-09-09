package main

import (
	"context"
	"encoding/json"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"testing"
)

func TestSiteAcknowledgementRequiresAppliedContentAndIdentity(t *testing.T) {
	for _, kind := range []string{"current", "node", "key", "secret", "content"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "secret"}, Data: map[string][]byte{tunnel.NodePublicKeyPrefix + "w1": []byte("site-key"), tunnel.PeerPublicKeyPrefix + "remote": []byte("remote-key"), tunnel.PeerAllowedIPsPrefix + "remote": []byte("10.100.0.1/32"), tunnel.PeerRouteHostsPrefix + "remote": []byte("10.100.0.1")}}
			hash, err := tunnel.SitePeerHash(secret.Data)
			if err != nil {
				t.Fatal(err)
			}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1", UID: "node"}}
			switch kind {
			case "node":
				node.UID = "replacement"
			case "key":
				secret.Data[tunnel.NodePublicKeyPrefix+"w1"] = []byte("replacement")
			case "secret":
				secret.UID = "replacement"
			case "content":
				secret.Data[tunnel.PeerRouteHostsPrefix+"remote"] = []byte("10.100.0.2")
			}
			k := fake.NewClientset(secret, node)
			err = acknowledgeSite(ctx, k, "test", "mesh", "w1", "node", "site-key", "secret", hash)
			observed, getErr := k.CoreV1().Secrets("test").Get(ctx, "mesh", metav1.GetOptions{})
			if getErr != nil {
				t.Fatal(getErr)
			}
			raw := observed.Data[tunnel.SiteAppliedPrefix+"w1"]
			if kind != "current" {
				if err == nil || len(raw) > 0 {
					t.Fatal("acknowledged stale content or identity")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var ack tunnel.SiteApplied
			if err = json.Unmarshal(raw, &ack); err != nil || ack.Hash != hash || ack.NodeUID != "node" || ack.PublicKey != "site-key" {
				t.Fatalf("incomplete receipt %#v %v", ack, err)
			}
			if !tunnel.SiteConverged(observed.Data, "w1", "node") || tunnel.SiteConverged(observed.Data, "w1", "replacement") {
				t.Fatal("receipt identity verification failed")
			}
			// Writing the acknowledgement itself must not change the peer content hash.
			again, err := tunnel.SitePeerHash(observed.Data)
			if err != nil || again != hash {
				t.Fatal("acknowledgement changed desired content")
			}
		})
	}
}

func TestRelayedSiteReceiptTracksSelectedRelay(t *testing.T) {
	ctx := context.Background()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "mesh"}, Data: map[string][]byte{
		tunnel.NodePublicKeyPrefix + "cp": []byte("cp-key"), tunnel.NodeTunnelAddressPrefix + "cp": []byte("10.100.0.3"), tunnel.SiteAddressesPrefix + "cp": []byte("10.10.0.10"),
		tunnel.NodePublicKeyPrefix + "w1": []byte("w1-key"), tunnel.NodeTunnelAddressPrefix + "w1": []byte("10.100.0.1"), tunnel.NodeAddressesPrefix + "w1": []byte("10.10.0.11"),
		tunnel.NodePublicKeyPrefix + "w2": []byte("w2-key"), tunnel.NodeTunnelAddressPrefix + "w2": []byte("10.100.0.2"), tunnel.NodeAddressesPrefix + "w2": []byte("10.10.0.12"),
	}}
	ready := corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}
	cp := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp", UID: "cp"}, Status: ready}
	w1 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1", UID: "w1"}, Status: ready}
	w2 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w2", UID: "w2"}, Status: ready}
	k := fake.NewClientset(secret, cp, w1, w2)
	hash, err := tunnel.SitePeerHash(secret.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := acknowledgeSite(ctx, k, "test", "mesh", "cp", "cp", "cp-key", "mesh", hash); err == nil {
		t.Fatal("direct receipt accepted for relayed node")
	}
	transit, err := tunnel.SiteTransit(secret.Data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := acknowledgeSite(ctx, k, "test", "mesh", "cp", "cp", "cp-key", "mesh", hash, transit); err != nil {
		t.Fatal(err)
	}
	observed, err := k.CoreV1().Secrets("test").Get(ctx, "mesh", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !tunnel.SiteConverged(observed.Data, "cp", "cp") {
		t.Fatal("relayed receipt not recognized")
	}
	if tunnel.SiteConverged(observed.Data, "cp", "cp", map[string]bool{"w1": true}) {
		t.Fatal("old receipt survived relay failover")
	}
	delete(observed.Data, tunnel.NodePublicKeyPrefix+"cp")
	delete(observed.Data, tunnel.NodeTunnelAddressPrefix+"cp")
	if _, err := k.CoreV1().Secrets("test").Update(ctx, observed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if tunnel.SiteConverged(observed.Data, "cp", "cp") {
		t.Fatal("retained-endpoint receipt survived retirement")
	}
	if err := acknowledgeSite(ctx, k, "test", "mesh", "cp", "cp", "", "mesh", hash, transit); err != nil {
		t.Fatal(err)
	}
	observed, err = k.CoreV1().Secrets("test").Get(ctx, "mesh", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !tunnel.SiteConverged(observed.Data, "cp", "cp") {
		t.Fatal("non-endpoint receipt not recognized")
	}
	changed := *transit
	changed.Hosts = []string{"172.29.0.21"}
	if err := acknowledgeSite(ctx, k, "test", "mesh", "cp", "cp", "", "mesh", hash, &changed); err == nil {
		t.Fatal("different applied routes acknowledged")
	}
}
