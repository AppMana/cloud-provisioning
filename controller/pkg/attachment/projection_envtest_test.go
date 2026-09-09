package attachment

import (
	"context"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"net/netip"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type concurrentSecretUpdate struct {
	client.Client
	race bool
}

func (c *concurrentSecretUpdate) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.race {
		c.race = false
		other := &corev1.Secret{}
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(obj), other); err != nil {
			return err
		}
		other.Annotations = map[string]string{"concurrent-writer": "observed"}
		if err := c.Client.Update(ctx, other); err != nil {
			return err
		}
	}
	return c.Client.Update(ctx, obj, opts...)
}

// Real API concurrency and finalizer behavior cannot be established by the
// fake client. This starts dedicated API/etcd processes, never a user cluster.
func TestAPIRejectsConcurrentProjectionPublication(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for isolated API validation")
	}
	env := &envtest.Environment{}
	config, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "attachments"}}); err != nil {
		t.Fatal(err)
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "attachments"}, Data: map[string][]byte{}}
	for name, addr := range map[string]string{"gateway": "10.100.0.1", "worker": "10.100.0.2"} {
		secret.Data[tunnel.PeerPublicKeyPrefix+name] = []byte(name)
		secret.Data[tunnel.PeerAllowedIPsPrefix+name] = []byte(addr + "/32")
		secret.Data[tunnel.PeerRouteHostsPrefix+name] = []byte(addr)
	}
	if err := c.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	racing := &concurrentSecretUpdate{Client: c, race: true}
	store := ProjectionStore{Client: racing, Namespace: "attachments", Name: "mesh", SecretUID: string(secret.UID)}
	p := tunnel.GatewayProjection{Lease: "worker/epoch", WorkerKey: "worker", GatewayKey: "gateway", WorkerAddress: netip.MustParseAddr("172.29.0.21"), WorkerHost: netip.MustParsePrefix("172.29.0.21/32")}
	if _, err := store.Set(ctx, p.Lease, &p); !apierrors.IsConflict(err) {
		t.Fatalf("expected API conflict: %v", err)
	}
	current := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(secret), current); err != nil {
		t.Fatal(err)
	}
	if len(current.Data[tunnel.GatewayProjectionsKey]) != 0 || current.Annotations["concurrent-writer"] != "observed" {
		t.Fatal("conflict lost another writer's state")
	}
	published, err := store.Set(ctx, p.Lease, &p)
	if err != nil {
		t.Fatal(err)
	}
	if len(published.Data[tunnel.GatewayProjectionsKey]) == 0 || published.Annotations["concurrent-writer"] != "observed" {
		t.Fatal("retry did not preserve current state")
	}
	if _, err := store.Set(ctx, p.Lease, nil); err != nil {
		t.Fatal(err)
	}
}
