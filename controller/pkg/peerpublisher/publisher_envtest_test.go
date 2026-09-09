package peerpublisher

import (
	"context"
	"os"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type callbackDelivery func(string, []byte) (bool, error)

func (f callbackDelivery) Submit(uid string, raw []byte) (bool, error) { return f(uid, raw) }

// Exercise API resourceVersion enforcement, which the fake client does not
// implement. No production kubeconfig is read; envtest starts its own API/etcd.
func TestAPIServerRejectsAcknowledgmentOfConcurrentUpdate(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for the isolated API test")
	}
	environment := &envtest.Environment{}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "delivery-test"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	secrets := client.CoreV1().Secrets("delivery-test")
	original, err := secrets.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "adoption"}, Data: map[string][]byte{tunnel.CloudPeersKey: []byte(`{"peers":[]}`)}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := Publisher{Secrets: secrets, Name: original.Name}
	p.Delivery = callbackDelivery(func(uid string, raw []byte) (bool, error) {
		changed := original.DeepCopy()
		changed.Data[tunnel.CloudPeersKey] = []byte("{\n\"peers\":[]\n}")
		_, err := secrets.Update(ctx, changed, metav1.UpdateOptions{})
		return true, err
	})
	if err = p.Reconcile(ctx); !apierrors.IsConflict(err) {
		t.Fatalf("expected concurrent-update conflict, got %v", err)
	}
	current, err := secrets.Get(ctx, original.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if current.Annotations[tunnel.AppliedListAnnotation] != "" {
		t.Fatal("stale delivery acknowledged changed Secret")
	}
	p.Delivery = callbackDelivery(func(uid string, raw []byte) (bool, error) { return true, nil })
	if err = p.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	current, err = secrets.Get(ctx, original.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if current.Annotations[tunnel.AppliedListAnnotation] != tunnel.HashPeerList(current.Data[tunnel.CloudPeersKey]) {
		t.Fatal("current applied delivery not acknowledged")
	}
}
