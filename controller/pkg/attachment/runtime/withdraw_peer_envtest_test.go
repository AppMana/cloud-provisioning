package runtime

import (
	"context"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	machine := &unstructured.Unstructured{}
	machine.SetAPIVersion("cluster.x-k8s.io/v1beta2")
	machine.SetKind("Machine")
	machine.SetNamespace("test")
	machine.SetName("publication-worker")
	if err := api.Create(ctx, machine); err != nil {
		t.Fatal(err)
	}
	observed := machine.DeepCopy()
	writer := mesh.DeepCopy()
	if err := attachment.CheckPeerPublication(ctx, api, observed); err != nil {
		t.Fatal(err)
	}
	writerPatch := client.MergeFromWithOptions(writer.DeepCopy(), client.MergeFromWithOptimisticLock{})
	writer.Data[tunnel.PeerEndpointPrefix+"worker"] = []byte("192.0.2.99:51820")
	machine.SetAnnotations(map[string]string{attachment.DrainIntentAnnotation: "group/action"})
	if err := api.Update(ctx, machine); err != nil {
		t.Fatal(err)
	}
	if err := attachment.CheckPeerPublication(ctx, api, observed); err == nil {
		t.Fatal("old Machine observation bypassed drain marker")
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
	if err := api.Patch(ctx, writer, writerPatch); !apierrors.IsConflict(err) {
		t.Fatalf("in-flight writer restored withdrawn peer: %v", err)
	}
	replacementObservation := observed.DeepCopy()
	replacementObservation.SetUID("different-machine")
	if err := attachment.CheckPeerPublication(ctx, api, replacementObservation); err == nil {
		t.Fatal("replacement Machine accepted")
	}
	if err := attachment.CheckPeerPublication(ctx, api, &unstructured.Unstructured{}); err == nil {
		t.Fatal("missing identity accepted")
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
