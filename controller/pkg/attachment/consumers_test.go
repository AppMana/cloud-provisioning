package attachment

import (
	"context"
	"encoding/json"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestConsumerVerifierRequiresExactDocumentsAndIdentities(t *testing.T) {
	for _, kind := range []string{"current", "old-document", "old-ack", "missing", "replacement-node", "replacement-secret", "site-ack", "changed-api"} {
		t.Run(kind, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "mesh", UID: "mesh"}, Data: map[string][]byte{tunnel.NodePublicKeyPrefix + "site": []byte("site-key")}}
			hash, err := tunnel.SitePeerHash(mesh.Data)
			if err != nil {
				t.Fatal(err)
			}
			ack, _ := json.Marshal(tunnel.SiteApplied{NodeUID: "site", PublicKey: "site-key", Hash: hash})
			mesh.Data[tunnel.SiteAppliedPrefix+"site"] = ack
			doc := []byte(`{"peers":[],"apiServers":["10.10.0.10:6443"]}`)
			remote := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "remote", UID: "remote-secret", Annotations: map[string]string{tunnel.AppliedListAnnotation: tunnel.HashPeerList(doc)}}, Data: map[string][]byte{tunnel.CloudPeersKey: doc}}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "remote", UID: "remote-node"}}
			consumers := []Consumer{{NodeName: "site", NodeUID: "site", Site: true}, {NodeName: "remote", NodeUID: "remote-node", SecretName: "remote", SecretUID: "remote-secret", Document: doc}}
			snapshot := mesh.DeepCopy()
			switch kind {
			case "changed-api":
				mesh.Data[tunnel.APIServersKey] = []byte("10.10.0.99")
			case "old-document":
				remote.Data[tunnel.CloudPeersKey] = []byte(`{"peers":[]}`)
				remote.Annotations[tunnel.AppliedListAnnotation] = tunnel.HashPeerList(remote.Data[tunnel.CloudPeersKey])
			case "old-ack":
				remote.Annotations[tunnel.AppliedListAnnotation] = "old"
			case "replacement-node":
				node.UID = "new"
			case "replacement-secret":
				remote.UID = "new"
			case "site-ack":
				delete(mesh.Data, tunnel.SiteAppliedPrefix+"site")
			}
			objects := []client.Object{mesh, node, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "site", UID: "site"}}}
			if kind != "missing" {
				objects = append(objects, remote)
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			ok, err := (ConsumerVerifier{Reader: reader}).Applied(context.Background(), snapshot, consumers)
			if err != nil || ok != (kind == "current") {
				t.Fatalf("applied %v %v", ok, err)
			}
		})
	}
}
