package attachment

import (
	"context"
	"encoding/json"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"net/netip"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestProjectionStoreSharesGatewayAndRejectsReplacement(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "mesh", UID: "mesh-uid"}, Data: map[string][]byte{"unrelated": []byte("preserve")}}
	for name, addr := range map[string]string{"gateway": "10.100.0.1", "worker1": "10.100.0.2", "worker2": "10.100.0.3"} {
		secret.Data[tunnel.PeerPublicKeyPrefix+name] = []byte(name)
		secret.Data[tunnel.PeerAllowedIPsPrefix+name] = []byte(addr + "/32")
		secret.Data[tunnel.PeerRouteHostsPrefix+name] = []byte(addr)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	store := ProjectionStore{Client: c, Namespace: "test", Name: "mesh", SecretUID: "mesh-uid"}
	ctx := context.Background()
	projection := func(lease, key, ip string) tunnel.GatewayProjection {
		a := netip.MustParseAddr(ip)
		return tunnel.GatewayProjection{Lease: lease, WorkerKey: key, GatewayKey: "gateway", WorkerAddress: a, WorkerHost: netip.PrefixFrom(a, 32)}
	}
	first := projection("lease1", "worker1", "172.29.0.21")
	second := projection("lease2", "worker2", "172.29.0.13")
	for _, p := range []tunnel.GatewayProjection{first, second} {
		if _, err := store.Set(ctx, p.Lease, &p); err != nil {
			t.Fatal(err)
		}
	}
	changed := first
	changed.GatewayKey = "worker2"
	if _, err := store.Set(ctx, first.Lease, &changed); err == nil {
		t.Fatal("overwrote active binding")
	}
	staged, present, err := store.RetireWorker(ctx, first.Lease)
	if err != nil || !present {
		t.Fatal("failed to stage worker withdrawal", err)
	}
	var stagedPlans []tunnel.GatewayProjection
	if err := json.Unmarshal(staged.Data[tunnel.GatewayProjectionsKey], &stagedPlans); err != nil || len(stagedPlans) != 2 || !stagedPlans[0].RetiringWorker || stagedPlans[1].RetiringWorker {
		t.Fatal("staging changed another worker", err)
	}
	if _, err := store.Set(ctx, first.Lease, &first); err == nil {
		t.Fatal("worker withdrawal was reversed")
	}
	if _, present, err := store.RetireWorker(ctx, first.Lease); err != nil || !present {
		t.Fatal("worker withdrawal did not survive a retry", err)
	}
	snapshot, err := store.Set(ctx, first.Lease, nil)
	if err != nil {
		t.Fatal(err)
	}
	var remaining []tunnel.GatewayProjection
	if err = json.Unmarshal(snapshot.Data[tunnel.GatewayProjectionsKey], &remaining); err != nil || len(remaining) != 1 || remaining[0].Lease != second.Lease || string(snapshot.Data["unrelated"]) != "preserve" {
		t.Fatal("withdrawal removed other state")
	}
	if _, present, err := store.RetireWorker(ctx, first.Lease); err != nil || present {
		t.Fatal("recreated a globally withdrawn projection", err)
	}
	first.Lease = "replacement"
	if _, err := store.Set(ctx, first.Lease, &first); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Set(ctx, "lease1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(snapshot.Data[tunnel.GatewayProjectionsKey], &remaining); err != nil || len(remaining) != 2 {
		t.Fatal("stale withdrawal removed replacement")
	}
	store.SecretUID = "different"
	if _, err := store.Set(ctx, second.Lease, nil); err == nil {
		t.Fatal("adopted a replacement mesh")
	}
}
