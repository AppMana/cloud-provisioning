package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// No LIST implementation: these reads must work with named-GET-only RBAC.
type releasedLoadMaps struct {
	typedcore.ConfigMapInterface
	get func(context.Context, string) (*corev1.ConfigMap, error)
}

func (m releasedLoadMaps) Get(ctx context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	return m.get(ctx, name)
}

// These synthetic journals test loading and history validation only. Release
// finalization and local ownership authentication remain separate admission.
func releasedLoadFixture(t *testing.T, count int) (handover.Store, tunnel.ReceiveInventory, map[string]*corev1.ConfigMap) {
	t.Helper()
	key := func() string {
		k, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		return k.PublicKey().String()
	}
	tr, err := handover.NewTransition(handover.Spec{ClusterUID: "cluster", TransitionUID: "transition", PlanHash: strings.Repeat("a", 64), FromGeneration: "A", ToGeneration: "B", Phase: handover.Prepare, Members: []handover.Member{{NodeUID: "node", FromKey: key(), ToKey: key()}}}, time.Now(), handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tr.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	store := handover.Store{ClusterUID: "cluster"}
	inventory := tunnel.ReceiveInventory{Released: map[string]tunnel.ReceiveJournalPublication{}}
	objects := map[string]*corev1.ConfigMap{}
	for i := range count {
		uid := fmt.Sprintf("machine-%d", i)
		name, journalUID := tunnel.ReceiveTransitionName(uid), fmt.Sprintf("journal-%d", i)
		inventory.Released[uid] = tunnel.ReceiveJournalPublication{JournalUID: journalUID}
		objects[name] = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "mesh", UID: types.UID(journalUID), ResourceVersion: "opaque"}, Data: map[string]string{"transition.json": string(raw)}}
	}
	return store, inventory, objects
}

func TestReleasedJournalLoadsBoundIndependentNamedReads(t *testing.T) {
	store, inventory, objects := releasedLoadFixture(t, 24)
	var active, peak atomic.Int32
	var mu sync.Mutex
	counts := map[string]int{}
	arrived, release := make(chan struct{}, len(objects)), make(chan struct{})
	store.ConfigMaps = releasedLoadMaps{get: func(ctx context.Context, name string) (*corev1.ConfigMap, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		mu.Lock()
		counts[name]++
		mu.Unlock()
		arrived <- struct{}{}
		select {
		case <-release:
			return objects[name].DeepCopy(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		snapshots map[string]handover.Snapshot
		err       error
	}
	done := make(chan result, 1)
	go func() {
		snapshots, err := loadReleasedReceiveJournals(ctx, store, inventory)
		done <- result{snapshots, err}
	}()
	for range 4 {
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatal("independent reads were serialized")
		}
	}
	close(release)
	r := <-done
	if r.err != nil || len(r.snapshots) != len(objects) || peak.Load() != 4 || active.Load() != 0 {
		t.Fatalf("snapshots=%d peak=%d active=%d err=%v", len(r.snapshots), peak.Load(), active.Load(), r.err)
	}
	for uid, publication := range inventory.Released {
		snapshot := r.snapshots[uid]
		if counts[tunnel.ReceiveTransitionName(uid)] != 1 || string(snapshot.Metadata().UID) != publication.JournalUID {
			t.Fatal("named read or admitted incarnation lost")
		}
		if _, err := snapshot.View(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReleasedJournalLoadsRejectPartialOrInvalidSets(t *testing.T) {
	for _, failure := range []string{"read-error", "replacement", "wrong-name", "missing-version", "invalid-history", "wrong-cluster", "plan-mismatch"} {
		t.Run(failure, func(t *testing.T) {
			store, inventory, objects := releasedLoadFixture(t, 8)
			var active atomic.Int32
			store.ConfigMaps = releasedLoadMaps{get: func(ctx context.Context, name string) (*corev1.ConfigMap, error) {
				active.Add(1)
				defer active.Add(-1)
				object := objects[name].DeepCopy()
				if name == tunnel.ReceiveTransitionName("machine-3") {
					switch failure {
					case "read-error":
						return nil, errors.New("denied or unavailable")
					case "replacement":
						object.UID = "replacement"
					case "wrong-name":
						object.Name = "wrong"
					case "missing-version":
						object.ResourceVersion = ""
					case "invalid-history":
						object.Data["transition.json"] = "{}"
					case "wrong-cluster":
						object.Data["transition.json"] = strings.ReplaceAll(object.Data["transition.json"], `"clusterUID":"cluster"`, `"clusterUID":"other"`)
					case "plan-mismatch":
						object.Data["plan.json"] = "{}"
					}
				}
				return object, nil
			}}
			snapshots, err := loadReleasedReceiveJournals(context.Background(), store, inventory)
			if err == nil || snapshots != nil || active.Load() != 0 {
				t.Fatalf("partial or invalid set escaped: snapshots=%d active=%d err=%v", len(snapshots), active.Load(), err)
			}
		})
	}
}

func TestReleasedJournalLoadsJoinCancelledReads(t *testing.T) {
	store, inventory, _ := releasedLoadFixture(t, 24)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var active atomic.Int32
	arrived := make(chan struct{}, 24)
	store.ConfigMaps = releasedLoadMaps{get: func(ctx context.Context, _ string) (*corev1.ConfigMap, error) {
		active.Add(1)
		defer active.Add(-1)
		arrived <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	done := make(chan error, 1)
	go func() {
		snapshots, err := loadReleasedReceiveJournals(ctx, store, inventory)
		if snapshots != nil {
			done <- errors.New("cancelled load returned partial snapshots")
			return
		}
		done <- err
	}()
	for range 4 {
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatal("reads did not reach concurrency bound")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || active.Load() != 0 {
		t.Fatalf("reads survived cancellation: active=%d err=%v", active.Load(), err)
	}
	store.ConfigMaps = releasedLoadMaps{get: func(context.Context, string) (*corev1.ConfigMap, error) {
		t.Error("read after cancellation")
		return nil, context.Canceled
	}}
	if snapshots, err := loadReleasedReceiveJournals(ctx, store, inventory); snapshots != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancelled load was accepted")
	}
}
