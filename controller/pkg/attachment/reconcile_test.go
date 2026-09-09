package attachment

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type backend struct {
	calls                                    []string
	leases                                   []string
	forwarding, applied, withdrawn, released bool
	failure                                  error
}

func (b *backend) call(name string, r Record, ready bool) (bool, error) {
	b.calls = append(b.calls, name)
	b.leases = append(b.leases, r.LeaseID())
	return ready, b.failure
}
func (b *backend) Ensure(_ context.Context, r Record) (bool, error) {
	return b.call("ensure", r, b.forwarding)
}
func (b *backend) Release(_ context.Context, r Record) (bool, error) {
	return b.call("release", r, b.released)
}
func (b *backend) Publish(_ context.Context, r Record) (bool, error) {
	return b.call("publish", r, b.applied)
}
func (b *backend) Withdraw(_ context.Context, r Record) (bool, error) {
	return b.call("withdraw", r, b.withdrawn)
}

type interruptedStore struct {
	Store
	fail bool
}

func (s *interruptedStore) Save(ctx context.Context, old, next *Record) error {
	if s.fail {
		s.fail = false
		return fmt.Errorf("persistence interrupted")
	}
	return s.Store.Save(ctx, old, next)
}

func fixture(t *testing.T) (Reconciler, *backend, *interruptedStore) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := &interruptedStore{Store: ConfigMapStore{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "test"}}
	b := &backend{}
	return Reconciler{Store: store, Forwarder: b, Publication: b}, b, store
}

func TestPersistedIntentSurvivesInterruptedPreparation(t *testing.T) {
	ctx := context.Background()
	r, b, store := fixture(t)
	desired := observedRequest()
	// No cloud operation may precede durable intent.
	store.fail = true
	if _, err := r.Step(ctx, "worker", &desired); err == nil || len(b.calls) != 0 {
		t.Fatal("unrecorded cloud mutation")
	}
	if p, err := r.Step(ctx, "worker", &desired); err != nil || p != Preparing {
		t.Fatalf("%s %v", p, err)
	}
	b.forwarding = true
	store.fail = true
	if _, err := r.Step(ctx, "worker", &desired); err == nil {
		t.Fatal("missing persistence failure")
	}
	recorded, err := store.Load(ctx, "worker")
	if err != nil || recorded.Phase != Preparing {
		t.Fatal("preparation intent lost")
	}
	// A new reconciler resumes the same lease after the provider already ran.
	r = Reconciler{Store: store, Forwarder: b, Publication: b}
	if p, err := r.Step(ctx, "worker", &desired); err != nil || p != Publishing {
		t.Fatalf("%s %v", p, err)
	}
	if len(b.leases) != 2 || b.leases[0] != b.leases[1] || !reflect.DeepEqual(b.calls, []string{"ensure", "ensure"}) {
		t.Fatal("retry created a new provider lease")
	}
	if p, err := r.Step(ctx, "worker", &desired); err != nil || p != Publishing {
		t.Fatal("unacknowledged publication became ready")
	}
	b.applied = true
	if p, err := r.Step(ctx, "worker", &desired); err != nil || p != Ready {
		t.Fatalf("%s %v", p, err)
	}
	b.failure = fmt.Errorf("gateway observation unavailable")
	if p, err := r.Step(ctx, "worker", &desired); err == nil || p != Publishing {
		t.Fatal("stale Ready survived failed observation")
	}
}

func TestReplacementWaitsForWithdrawalAndReleasesOriginalIdentity(t *testing.T) {
	ctx := context.Background()
	r, b, store := fixture(t)
	desired := observedRequest()
	b.forwarding = true
	b.applied = true
	for _, want := range []Phase{Preparing, Publishing, Ready} {
		if got, err := r.Step(ctx, "worker", &desired); err != nil || got != want {
			t.Fatalf("%s %v", got, err)
		}
	}
	old, _ := store.Load(ctx, "worker")
	desired.Gateway.UID = "replacement-machine"
	desired.Gateway.NodeUID = "replacement-node"
	desired.Gateway.ProviderID = "aws:///zone/i-replacement"
	desired.Gateway.InterfaceID = "eni-replacement"
	b.calls = nil
	b.leases = nil
	if got, err := r.Step(ctx, "worker", &desired); err != nil || got != Withdrawing || len(b.calls) != 0 {
		t.Fatal("withdrawal was not persisted first")
	}
	for i := 0; i < 3; i++ {
		if got, err := r.Step(ctx, "worker", &desired); err != nil || got != Withdrawing {
			t.Fatal("released before acknowledgement")
		}
	}
	if !reflect.DeepEqual(b.calls, []string{"withdraw", "withdraw", "withdraw"}) {
		t.Fatal("provider touched before withdrawal ack")
	}
	b.withdrawn = true
	if got, err := r.Step(ctx, "worker", &desired); err != nil || got != Releasing {
		t.Fatalf("%s %v", got, err)
	}
	if got, err := r.Step(ctx, "worker", &desired); err != nil || got != Releasing {
		t.Fatal("incomplete provider release was skipped")
	}
	b.released = true
	if got, err := r.Step(ctx, "worker", &desired); err != nil || got != Complete {
		t.Fatalf("%s %v", got, err)
	}
	for _, lease := range b.leases {
		if lease != old.LeaseID() {
			t.Fatal("retirement used replacement identity")
		}
	}
	if got, err := r.Step(ctx, "worker", &desired); err != nil || got != Preparing {
		t.Fatalf("%s %v", got, err)
	}
	next, _ := store.Load(ctx, "worker")
	if next.Digest == old.Digest || next.Plan.Gateway.UID != desired.Gateway.UID {
		t.Fatal("replacement retained old lease")
	}
}

func TestDeletingIntentWithdrawsEvenWithInvalidDesired(t *testing.T) {
	ctx := context.Background()
	r, b, store := fixture(t)
	desired := observedRequest()
	if _, err := r.Step(ctx, "worker", &desired); err != nil {
		t.Fatal(err)
	}
	cms := store.Store.(ConfigMapStore)
	cm := &corev1.ConfigMap{}
	if err := cms.Client.Get(ctx, cms.key("worker"), cm); err != nil {
		t.Fatal(err)
	}
	if err := cms.Client.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}
	desired.Worker.UID = "" // Cleanup must use persisted identity, not this input.
	if got, err := r.Step(ctx, "worker", &desired); err != nil || got != Withdrawing {
		t.Fatalf("%s %v", got, err)
	}
	if len(b.calls) != 0 {
		t.Fatal("cloud release before durable withdrawal")
	}
	b.withdrawn = true
	b.released = true
	for _, want := range []Phase{Releasing, Complete} {
		if got, err := r.Step(ctx, "worker", nil); err != nil || got != want {
			t.Fatalf("%s %v", got, err)
		}
	}
	if record, err := store.Load(ctx, "worker"); err != nil || record != nil {
		t.Fatal("completed deleted intent retained its finalizer")
	}
}

func TestStoreRejectsStaleWriter(t *testing.T) {
	ctx := context.Background()
	r, _, store := fixture(t)
	desired := observedRequest()
	if _, err := r.Step(ctx, "worker", &desired); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Load(ctx, "worker")
	newer := *before
	newer.Phase = Withdrawing
	if err := store.Save(ctx, before, &newer); err != nil {
		t.Fatal(err)
	}
	stale := *before
	stale.Phase = Ready
	if err := store.Save(ctx, before, &stale); err == nil {
		t.Fatal("stale writer erased retirement intent")
	}
	current, _ := store.Load(ctx, "worker")
	if current.Phase != Withdrawing {
		t.Fatal("retirement intent overwritten")
	}
}

func TestReattachmentGetsNewLeaseAfterIntentDeletion(t *testing.T) {
	ctx := context.Background()
	r, b, store := fixture(t)
	desired := observedRequest()
	if _, err := r.Step(ctx, "worker", &desired); err != nil {
		t.Fatal(err)
	}
	old, _ := store.Load(ctx, "worker")
	cms := store.Store.(ConfigMapStore)
	cm := &corev1.ConfigMap{}
	if err := cms.Client.Get(ctx, cms.key("worker"), cm); err != nil {
		t.Fatal(err)
	}
	if err := cms.Client.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}
	b.withdrawn = true
	b.released = true
	for _, want := range []Phase{Withdrawing, Releasing, Complete} {
		if got, err := r.Step(ctx, "worker", nil); err != nil || got != want {
			t.Fatalf("%s %v", got, err)
		}
	}
	if _, err := r.Step(ctx, "worker", &desired); err != nil {
		t.Fatal(err)
	}
	next, _ := store.Load(ctx, "worker")
	if next.Digest != old.Digest || next.LeaseID() == old.LeaseID() {
		t.Fatal("reattachment reused a retired provider lease")
	}
}
