package attachment

import (
	"context"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func membershipFixture(t *testing.T) (MeshConsumerResolver, Record, *corev1.Secret, ConsumerTarget) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gv := schema.GroupVersion{Group: "cluster.x-k8s.io", Version: "v1beta1"}
	scheme.AddKnownTypeWithName(gv.WithKind("Machine"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gv.WithKind("MachineList"), &unstructured.UnstructuredList{})
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "mesh"}, Data: map[string][]byte{}}
	objects := []client.Object{mesh}
	for _, name := range []string{"worker", "gateway", "other"} {
		m := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": gv.String(), "kind": "Machine",
			"metadata": map[string]any{"name": name, "namespace": "test", "uid": "machine-" + name,
				"annotations": map[string]any{"cloud-provisioning.appmana.com/wireguard-addr4": "10.100.0.2/24"}},
			"spec":   map[string]any{"providerID": "test://" + name},
			"status": map[string]any{"nodeRef": map[string]any{"name": name}},
		}}
		objects = append(objects, m,
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("node-" + name)}, Spec: corev1.NodeSpec{ProviderID: "test://" + name}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName(name), Namespace: "test", UID: types.UID("secret-" + name)}})
		mesh.Data[tunnel.PeerPublicKeyPrefix+name] = []byte("key-" + name)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := MeshConsumerResolver{Reader: c, Namespace: "test", SecretName: "mesh", SecretUID: "mesh"}
	record := Record{ID: "attachment", Lease: "epoch", Digest: "digest", Plan: GatewayPlan{
		Worker:  Machine{UID: "machine-worker", NodeUID: "node-worker", ProviderID: "test://worker"},
		Gateway: Machine{UID: "machine-gateway", NodeUID: "node-gateway", ProviderID: "test://gateway"},
	}}
	old := ConsumerTarget{NodeName: "other", NodeUID: "old-node", MachineName: "other", SecretUID: "old-secret", PublicKey: "old-key", TunnelAddress: "10.100.0.9"}
	return r, record, mesh, old
}

func TestDeletingConsumerRequiresPairedLifetimeHooks(t *testing.T) {
	ctx := context.Background()
	r, record, _, _ := membershipFixture(t)
	c := r.Reader.(client.Client)
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Machine"})
	if err := c.Get(ctx, client.ObjectKey{Namespace: "test", Name: "worker"}, m); err != nil {
		t.Fatal(err)
	}
	drain, terminate := DeletionHookKeys(record.LeaseID())
	a := m.GetAnnotations()
	a[drain] = record.LeaseID()
	a[terminate] = record.LeaseID()
	m.SetAnnotations(a)
	m.SetFinalizers([]string{"test/keep"})
	if err := c.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, m); err != nil {
		t.Fatal(err)
	}
	if intent, err := r.Resolve(ctx, record); err != nil || len(intent.Consumers) != 3 {
		t.Fatal("held deleting consumer must still acknowledge", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(m), m); err != nil {
		t.Fatal(err)
	}
	a = m.GetAnnotations()
	delete(a, terminate)
	m.SetAnnotations(a)
	if err := c.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx, record); err == nil {
		t.Fatal("single pre-drain hook accepted as lifetime protection")
	}
}

func TestMembershipRequiresSameNameReplacementAndPreservesRetainedEvidence(t *testing.T) {
	r, record, mesh, old := membershipFixture(t)
	retained := []ConsumerTarget{old}
	got, err := r.Refresh(context.Background(), record, mesh, retained)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("current recipients omitted: %+v", got)
	}
	found := false
	for _, target := range got {
		if target.NodeName == "other" {
			found = target.NodeUID == "node-other" && target.SecretUID == "secret-other" && target.PublicKey == "key-other"
		}
	}
	if !found || !reflect.DeepEqual(retained, []ConsumerTarget{old}) {
		t.Fatal("replacement not required or retained evidence was changed")
	}
}

func TestRemoteRetirementRequiresAllRevocationEvidence(t *testing.T) {
	for _, condition := range []string{"node", "secret", "key", "site", "revoked"} {
		t.Run(condition, func(t *testing.T) {
			r, _, mesh, old := membershipFixture(t)
			c := r.Reader.(client.Client)
			switch condition {
			case "node":
				n := &corev1.Node{}
				if err := c.Get(context.Background(), client.ObjectKey{Name: "other"}, n); err != nil {
					t.Fatal(err)
				}
				// No Ready condition or publisher pod is present; neither is retirement.
				n.UID = types.UID(old.NodeUID)
				if err := c.Update(context.Background(), n); err != nil {
					t.Fatal(err)
				}
			case "secret":
				s := &corev1.Secret{}
				if err := c.Get(context.Background(), client.ObjectKey{Namespace: "test", Name: tunnel.AdoptionSecretName("other")}, s); err != nil {
					t.Fatal(err)
				}
				s.UID = types.UID(old.SecretUID)
				if err := c.Update(context.Background(), s); err != nil {
					t.Fatal(err)
				}
			case "key":
				mesh.Data[tunnel.PeerPublicKeyPrefix+"renamed"] = []byte(old.PublicKey)
			case "site":
				old.Site = true
			}
			err := r.retiredRemote(context.Background(), mesh, old)
			if (err == nil) != (condition == "revoked") {
				t.Fatalf("%s: %v", condition, err)
			}
		})
	}
}

func TestMembershipCannotRetireAnAttachmentParticipant(t *testing.T) {
	r, record, mesh, old := membershipFixture(t)
	record.Plan.Worker.UID = "deleted-worker-machine"
	if _, err := r.Refresh(context.Background(), record, mesh, []ConsumerTarget{old}); err == nil {
		t.Fatal("adopted a replacement for the immutable attachment participant")
	}
}

func TestMembershipPersistsNewRecipientsAndCannotForgetThemLater(t *testing.T) {
	ctx := context.Background()
	r, record, mesh, old := membershipFixture(t)
	c := r.Reader.(client.Client)
	store := ConfigMapPublicationStore{Client: c, Namespace: "test"}
	initial := &PublicationIntent{Lease: record.LeaseID(), Consumers: []ConsumerTarget{old}}
	if err := store.Save(ctx, nil, initial); err != nil {
		t.Fatal(err)
	}
	intent, err := store.Load(ctx, record.LeaseID())
	if err != nil {
		t.Fatal(err)
	}
	p := GatewayPublication{Store: store, Resolver: r}
	targets, updated, err := p.recipients(ctx, record, mesh, intent)
	if err != nil || len(targets) != 3 || len(updated.Consumers) != 4 || updated.Consumers[0] != old {
		t.Fatalf("recipient history not preserved and expanded: %+v %v", updated, err)
	}
	// A later mesh omission is insufficient: this newly recorded consumer's
	// Node and credential Secret still exist. It must not disappear from the gate.
	delete(mesh.Data, tunnel.PeerPublicKeyPrefix+"other")
	if err := c.Update(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.recipients(ctx, record, mesh, updated); err == nil {
		t.Fatal("new recipient was forgotten without retirement proof")
	}
	for _, mutate := range []func(*PublicationIntent){
		func(p *PublicationIntent) { p.Consumers = p.Consumers[1:] },
		func(p *PublicationIntent) { p.Consumers[0].SecretUID = "different" },
	} {
		next := *updated
		next.Consumers = append([]ConsumerTarget(nil), updated.Consumers...)
		mutate(&next)
		if err := store.Save(ctx, updated, &next); err == nil {
			t.Fatal("recipient history could be erased or replaced")
		}
	}
}
