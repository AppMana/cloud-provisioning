package main

import (
	"context"
	"os"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestSitePublicationCannotResurrectRetiredEndpoint(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("real API assets required")
	}
	api := &envtest.Environment{}
	rest, err := api.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := api.Stop(); err != nil {
			t.Error(err)
		}
	})
	client, err := kubernetes.NewForConfig(rest)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "publication"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{secretNamespace: "publication", secretName: "mesh", nodeName: "cp"}
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	secrets := client.CoreV1().Secrets(cfg.secretNamespace)
	allocated, err := secrets.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: cfg.secretName}, Data: map[string][]byte{tunnel.NodeTunnelAddressPrefix + cfg.nodeName: []byte("10.100.0.3/24")}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	retired := allocated.DeepCopy()
	delete(retired.Data, tunnel.NodeTunnelAddressPrefix+cfg.nodeName)
	retired.Data[tunnel.SiteAddressesPrefix+cfg.nodeName] = []byte("10.10.0.10")
	retired, err = secrets.Update(ctx, retired, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A publisher that read before retirement must lose the API version race.
	if err := publishNodeInfo(ctx, client, cfg, key.PublicKey(), allocated); !apierrors.IsConflict(err) {
		t.Fatalf("wanted conflict, got %v", err)
	}
	// A fresh transit observation must make no write at all.
	if err := publishNodeInfo(ctx, client, cfg, key.PublicKey(), retired); err != nil {
		t.Fatal(err)
	}
	current, err := secrets.Get(ctx, cfg.secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if current.ResourceVersion != retired.ResourceVersion || len(current.Data[tunnel.NodePublicKeyPrefix+cfg.nodeName]) != 0 {
		t.Fatal("retired key was republished")
	}
	// Selection can allocate the endpoint again; its persisted local key is reused.
	current.Data[tunnel.NodeTunnelAddressPrefix+cfg.nodeName] = []byte("10.100.0.3/24")
	current, err = secrets.Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := publishNodeInfo(ctx, client, cfg, key.PublicKey(), current); err != nil {
		t.Fatal(err)
	}
	current, err = secrets.Get(ctx, cfg.secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(current.Data[tunnel.NodePublicKeyPrefix+cfg.nodeName]) != key.PublicKey().String() {
		t.Fatal("allocated endpoint failed to publish")
	}
}
