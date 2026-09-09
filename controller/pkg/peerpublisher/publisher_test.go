package peerpublisher

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

type delivery struct {
	applied bool
	err     error
	uid     string
	raw     []byte
}

func (d *delivery) Submit(uid string, raw []byte) (bool, error) {
	d.uid = uid
	d.raw = raw
	return d.applied, d.err
}

func TestOnlyAppliedDeliveryAcknowledgesTheExactSecretVersion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		applied   bool
		err       error
		wantPatch bool
	}{
		{name: "file delivered"},
		{name: "native applied", applied: true, wantPatch: true},
		{name: "delivery failed", err: errors.New("host unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "adoption", Namespace: "mesh", UID: "unique-secret", ResourceVersion: "17"}, Data: map[string][]byte{tunnel.CloudPeersKey: []byte("{\n\"peers\":[]\n}")}}
			client := fake.NewClientset(secret)
			d := &delivery{applied: tc.applied, err: tc.err}
			p := Publisher{Secrets: client.CoreV1().Secrets("mesh"), Name: "adoption", Delivery: d}
			err := p.Reconcile(context.Background())
			if (err != nil) != (tc.err != nil) {
				t.Fatalf("unexpected error %v", err)
			}
			if d.uid != string(secret.UID) || string(d.raw) != string(secret.Data[tunnel.CloudPeersKey]) {
				t.Fatal("delivery lost Secret identity or exact payload")
			}
			patches := 0
			for _, action := range client.Actions() {
				patch, ok := action.(clienttesting.PatchAction)
				if !ok {
					continue
				}
				patches++
				var obj struct {
					Metadata struct {
						ResourceVersion string            `json:"resourceVersion"`
						UID             string            `json:"uid"`
						Annotations     map[string]string `json:"annotations"`
					} `json:"metadata"`
				}
				if err := json.Unmarshal(patch.GetPatch(), &obj); err != nil {
					t.Fatal(err)
				}
				if obj.Metadata.ResourceVersion != "17" || obj.Metadata.UID != "unique-secret" || obj.Metadata.Annotations[tunnel.AppliedListAnnotation] != tunnel.HashPeerList(d.raw) {
					t.Fatal("acknowledgment lacks exact payload or update preconditions")
				}
			}
			if (patches == 1) != tc.wantPatch {
				t.Fatalf("patches=%d, want acknowledgment=%v", patches, tc.wantPatch)
			}
		})
	}
}
