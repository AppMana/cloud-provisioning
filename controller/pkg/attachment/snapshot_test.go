package attachment

import (
	"bytes"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestSnapshotUsesSourceRenderAndRetainedIdentities(t *testing.T) {
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "mesh"}, Data: map[string][]byte{
		tunnel.NodePublicKeyPrefix + "site": []byte("site-key"), tunnel.NodeTunnelAddressPrefix + "site": []byte("10.100.0.1"),
		tunnel.PeerPublicKeyPrefix + "worker": []byte("worker-key"), tunnel.PeerRouteHostsPrefix + "worker": []byte("10.100.0.2"), tunnel.PeerAllowedIPsPrefix + "worker": []byte("10.100.0.2/32"),
		tunnel.APIServersKey: []byte("10.10.0.14,10.10.0.10,10.10.0.14")}}
	targets := []ConsumerTarget{{NodeName: "site", NodeUID: "site-uid", Site: true, PublicKey: "site-key"}, {NodeName: "worker", NodeUID: "worker-uid", MachineName: "worker", TunnelAddress: "10.100.0.2", PublicKey: "worker-key", SecretUID: "adoption-uid"}}
	consumers, err := SnapshotConsumers(mesh, targets, "10.10.0.10", "6443")
	if err != nil {
		t.Fatal(err)
	}
	want, err := tunnel.RemotePeerDocument(mesh.Data, "10.100.0.2", "10.10.0.10", "6443")
	if err != nil {
		t.Fatal(err)
	}
	if len(consumers) != 2 || !bytes.Equal(consumers[1].Document, want) || consumers[1].SecretUID != "adoption-uid" {
		t.Fatal("snapshot differs from controller render")
	}
	mesh.Data[tunnel.APIServersKey] = []byte("10.10.0.10,10.10.0.14")
	reordered, err := SnapshotConsumers(mesh, targets, "10.10.0.10", "6443")
	if err != nil || !bytes.Equal(reordered[1].Document, want) {
		t.Fatal("API set ordering changed snapshot")
	}
	mesh.Data[tunnel.APIServersKey] = []byte("10.10.0.10")
	changed, err := SnapshotConsumers(mesh, targets, "10.10.0.10", "6443")
	if err != nil || bytes.Equal(changed[1].Document, want) {
		t.Fatal("real API removal did not change expected document")
	}
	delete(mesh.Data, tunnel.PeerPublicKeyPrefix+"worker")
	if _, err := SnapshotConsumers(mesh, targets, "10.10.0.10", "6443"); err == nil {
		t.Fatal("silently dropped missing consumer")
	}
}
