package attachment

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"net/netip"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

type publicationResolver struct {
	intent *PublicationIntent
	fail   bool
}

func (r *publicationResolver) Resolve(context.Context, Record) (*PublicationIntent, error) {
	if r.fail {
		return nil, fmt.Errorf("Machine unavailable")
	}
	return r.intent, nil
}

type cniProbe struct{ ensures, restores int }

func (c *cniProbe) Ensure(context.Context, Record) (bool, error)  { c.ensures++; return true, nil }
func (c *cniProbe) Restore(context.Context, Record) (bool, error) { c.restores++; return true, nil }
func TestPublicationWaitsForBothAcknowledgementsAndRetainsRecipients(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "mesh"}, Data: map[string][]byte{tunnel.NodePublicKeyPrefix + "site": []byte("site-key"), tunnel.NodeTunnelAddressPrefix + "site": []byte("10.100.0.1")}}
	for name, addr := range map[string]string{"worker": "10.100.0.2", "gateway": "10.100.0.3"} {
		mesh.Data[tunnel.PeerPublicKeyPrefix+name] = []byte(name)
		mesh.Data[tunnel.PeerRouteHostsPrefix+name] = []byte(addr)
		mesh.Data[tunnel.PeerAllowedIPsPrefix+name] = []byte(addr + "/32")
	}
	remote := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName("worker"), Namespace: "test", UID: "remote"}}
	objects := []client.Object{mesh, remote}
	for _, name := range []string{"site", "worker"} {
		objects = append(objects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name)}})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	record := Record{ID: "attachment", Lease: "epoch", Digest: "digest"}
	targets := []ConsumerTarget{{NodeName: "site", NodeUID: "site", Site: true, PublicKey: "site-key"}, {NodeName: "worker", NodeUID: "worker", MachineName: "worker", TunnelAddress: "10.100.0.2", PublicKey: "worker", SecretUID: "remote"}}
	intent := &PublicationIntent{Lease: record.LeaseID(), Projection: tunnel.GatewayProjection{Lease: record.LeaseID(), WorkerKey: "worker", GatewayKey: "gateway", WorkerAddress: netip.MustParseAddr("172.29.0.21"), WorkerHost: netip.MustParsePrefix("172.29.0.21/32"), DirectHosts: []netip.Prefix{netip.MustParsePrefix("10.100.0.1/32")}}, Consumers: targets}
	resolver := &publicationResolver{intent: intent}
	transport := &cniProbe{}
	store := ConfigMapPublicationStore{Client: c, Namespace: "test"}
	p := GatewayPublication{Store: store, Resolver: resolver, Transport: transport, Projections: ProjectionStore{Client: c, Namespace: "test", Name: "mesh", SecretUID: "mesh"}, Verifier: ConsumerVerifier{Reader: c}, APIPort: "6443"}
	acknowledge := func() {
		current := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(mesh), current); err != nil {
			t.Fatal(err)
		}
		consumers, err := SnapshotConsumers(current, targets, "", "6443")
		if err != nil {
			t.Fatal(err)
		}
		hash, err := tunnel.SitePeerHash(current.Data)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(tunnel.SiteApplied{NodeUID: "site", PublicKey: "site-key", Hash: hash})
		current.Data[tunnel.SiteAppliedPrefix+"site"] = raw
		if err := c.Update(ctx, current); err != nil {
			t.Fatal(err)
		}
		adopted := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(remote), adopted); err != nil {
			t.Fatal(err)
		}
		adopted.Data = map[string][]byte{tunnel.CloudPeersKey: consumers[1].Document}
		adopted.Annotations = map[string]string{tunnel.AppliedListAnnotation: tunnel.HashPeerList(consumers[1].Document)}
		if err := c.Update(ctx, adopted); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := p.Publish(ctx, record); err != nil || ok || transport.ensures != 0 {
		t.Fatalf("premature publication %v %v", ok, err)
	}
	resolver.fail = true // Subsequent passes and withdrawal use persisted identities.
	acknowledge()
	if ok, err := p.Publish(ctx, record); err != nil || !ok || transport.ensures != 1 {
		t.Fatalf("publish %v %v", ok, err)
	}
	if ok, err := p.Withdraw(ctx, record); err != nil || ok {
		t.Fatalf("old receipt authorized withdrawal %v %v", ok, err)
	}
	if transport.restores != 0 {
		t.Fatal("changed native transport before worker fallback acknowledgement")
	}
	staged := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(mesh), staged); err != nil {
		t.Fatal(err)
	}
	var stagedPlans []tunnel.GatewayProjection
	if err := json.Unmarshal(staged.Data[tunnel.GatewayProjectionsKey], &stagedPlans); err != nil || len(stagedPlans) != 1 || !stagedPlans[0].RetiringWorker {
		t.Fatal("worker fallback must precede site withdrawal", err)
	}
	saved, err := store.Load(ctx, record.LeaseID())
	if err != nil || saved.Retired || len(saved.Consumers) != 2 {
		t.Fatal("retired or lost recipients before acknowledgement")
	}
	acknowledge()
	if ok, err := p.Withdraw(ctx, record); err != nil || ok {
		t.Fatalf("worker acknowledgement alone authorized global withdrawal %v %v", ok, err)
	}
	acknowledge()
	if ok, err := p.Withdraw(ctx, record); err != nil || !ok {
		t.Fatalf("withdraw %v %v", ok, err)
	}
	saved, err = store.Load(ctx, record.LeaseID())
	if err != nil || !saved.Retired {
		t.Fatal("retirement not persisted")
	}
}
