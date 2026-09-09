package runtime

import (
	"context"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
)

func checkAPIWithdrawalPublication(t *testing.T, ctx context.Context, api client.Client) {
	t.Helper()
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "withdrawal-source", Namespace: "test"}, Data: map[string][]byte{tunnel.PeerPublicKeyPrefix + "worker": []byte("original-key"), tunnel.PeerEndpointPrefix + "worker": []byte("192.0.2.1:51820"), tunnel.PeerPublicKeyPrefix + "survivor": []byte("survivor-key"), tunnel.TunnelAddressReservationPrefix + "worker": []byte("10.100.0.2")}}
	if err := api.Create(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	stale := mesh.DeepCopy()
	mesh.Data[tunnel.PeerEndpointPrefix+"survivor"] = []byte("192.0.2.2:51820")
	if err := api.Update(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.PublishRemoteWithdrawal(ctx, api, stale, "worker", "original-key"); !apierrors.IsConflict(err) {
		t.Fatalf("stale source overwrote concurrent peer: %v", err)
	}
	published, err := attachment.PublishRemoteWithdrawal(ctx, api, mesh, "worker", "original-key")
	if err != nil {
		t.Fatal(err)
	}
	if string(mesh.Data[tunnel.PeerPublicKeyPrefix+"worker"]) != "original-key" {
		t.Fatal("publication mutated input snapshot")
	}
	if len(published.Data[tunnel.PeerPublicKeyPrefix+"worker"]) != 0 || string(published.Data[tunnel.PeerEndpointPrefix+"survivor"]) != "192.0.2.2:51820" || string(published.Data[tunnel.TunnelAddressReservationPrefix+"worker"]) != "10.100.0.2" {
		t.Fatal("withdrawal changed survivor or reservation")
	}
	withdrawn := published.DeepCopy()
	published.Data[tunnel.PeerPublicKeyPrefix+"worker"] = []byte("replacement-key")
	if err := api.Update(ctx, published); err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.PublishRemoteWithdrawal(ctx, api, withdrawn, "worker", "original-key"); !apierrors.IsConflict(err) {
		t.Fatalf("absent-peer retry removed replacement: %v", err)
	}
	if _, err := attachment.PublishRemoteWithdrawal(ctx, api, published, "worker", "original-key"); err == nil {
		t.Fatal("wrong key removed replacement")
	}
}
