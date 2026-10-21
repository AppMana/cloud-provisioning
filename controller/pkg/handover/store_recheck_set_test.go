package handover

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Only named GET is implemented. Broadening admission to LIST must not silently
// acquire permissions unavailable to the deployed agent.
type recheckNamedMaps struct {
	typedcore.ConfigMapInterface
	get func(context.Context, string) (*corev1.ConfigMap, error)
}

func (m recheckNamedMaps) Get(ctx context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	return m.get(ctx, name)
}

func recheckSnapshots(t *testing.T, count int) (Store, []Snapshot) {
	t.Helper()
	store, original := checkpointFixture(t, transitionFixture(t))
	result := make([]Snapshot, count)
	for i := range result {
		object := original.object.DeepCopy()
		object.Name, object.UID = fmt.Sprintf("journal-%d", i), types.UID(fmt.Sprintf("uid-%d", i))
		member := store
		member.Name = object.Name
		var err error
		result[i], err = member.snapshot(object)
		if err != nil {
			t.Fatal(err)
		}
	}
	return store, result
}

func TestRecheckExecutionsBoundsIndependentNamedReads(t *testing.T) {
	store, snapshots := recheckSnapshots(t, 22)
	// A set reader has no selected journal until it visits an admitted member.
	store.Name, store.ExpectedUID = "", ""
	objects := map[string]*corev1.ConfigMap{}
	for _, snapshot := range snapshots {
		objects[snapshot.object.Name] = snapshot.object
	}
	var active, peak atomic.Int32
	var mu sync.Mutex
	counts := map[string]int{}
	arrived := make(chan struct{}, len(snapshots))
	release := make(chan struct{})
	store.ConfigMaps = recheckNamedMaps{get: func(ctx context.Context, name string) (*corev1.ConfigMap, error) {
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
	done := make(chan error, 1)
	go func() { done <- store.RecheckExecutions(ctx, snapshots) }()
	for range 4 {
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatal("independent journal reads were serialized")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 4 || active.Load() != 0 {
		t.Fatalf("peak=%d active after return=%d", peak.Load(), active.Load())
	}
	for name := range objects {
		if counts[name] != 1 {
			t.Fatalf("%s live reads=%d", name, counts[name])
		}
	}
}

func TestRecheckExecutionsRejectsInvalidAdmissionBeforeReading(t *testing.T) {
	for _, invalid := range []string{"empty", "duplicate", "cluster", "deleted", "missing-reader", "cancelled"} {
		t.Run(invalid, func(t *testing.T) {
			store, snapshots := recheckSnapshots(t, 2)
			store.ConfigMaps = recheckNamedMaps{get: func(context.Context, string) (*corev1.ConfigMap, error) {
				t.Error("read started with invalid admission")
				return nil, errors.New("unexpected read")
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch invalid {
			case "empty":
				snapshots[1] = Snapshot{}
			case "duplicate":
				snapshots[1] = snapshots[0]
			case "cluster":
				store.ClusterUID = "other-cluster"
			case "deleted":
				now := metav1.Now()
				snapshots[1].object.DeletionTimestamp = &now
			case "missing-reader":
				store.ConfigMaps = nil
			case "cancelled":
				cancel()
			}
			if err := store.RecheckExecutions(ctx, snapshots); err == nil {
				t.Fatal("invalid admission accepted")
			}
		})
	}
}

func TestRecheckExecutionsRejectsAnyChangedMember(t *testing.T) {
	for _, mutation := range []string{"replaced", "missing", "deleting", "changed-plan", "advance", "observations", "denied"} {
		t.Run(mutation, func(t *testing.T) {
			store, snapshots := recheckSnapshots(t, 6)
			objects := map[string]*corev1.ConfigMap{}
			for _, snapshot := range snapshots {
				objects[snapshot.object.Name] = snapshot.object.DeepCopy()
			}
			changed := objects[snapshots[4].object.Name]
			switch mutation {
			case "replaced":
				changed.UID = "replacement"
			case "deleting":
				now := metav1.Now()
				changed.DeletionTimestamp = &now
			case "changed-plan":
				changed.Data[planKey] = "changed"
			case "advance":
				next := step(t, snapshots[4].Transition(), epoch.Add(time.Second))
				raw, err := next.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				changed.Data[journalKey] = string(raw)
			case "observations":
				prior := snapshots[4].Transition()
				at := epoch.Add(time.Second)
				next, err := prior.Observe(at, receipts(t, view(t, prior).Round, at)[:1])
				if err != nil {
					t.Fatal(err)
				}
				raw, err := next.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				changed.Data[journalKey] = string(raw)
			}
			store.ConfigMaps = recheckNamedMaps{get: func(_ context.Context, name string) (*corev1.ConfigMap, error) {
				if name == changed.Name {
					if mutation == "missing" {
						return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
					}
					if mutation == "denied" {
						return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, name, errors.New("denied"))
					}
				}
				return objects[name].DeepCopy(), nil
			}}
			err := store.RecheckExecutions(context.Background(), snapshots)
			if (err == nil) != (mutation == "observations") {
				t.Fatalf("mutation %s: %v", mutation, err)
			}
		})
	}
}

func TestRecheckExecutionsCancellationWaitsForReads(t *testing.T) {
	store, snapshots := recheckSnapshots(t, 22)
	var active, reads atomic.Int32
	arrived := make(chan struct{}, 4)
	store.ConfigMaps = recheckNamedMaps{get: func(ctx context.Context, _ string) (*corev1.ConfigMap, error) {
		active.Add(1)
		reads.Add(1)
		defer active.Add(-1)
		arrived <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- store.RecheckExecutions(ctx, snapshots) }()
	for range 4 {
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatal("reads did not start")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if active.Load() != 0 || reads.Load() != 4 {
		t.Fatalf("active=%d reads=%d after cancellation", active.Load(), reads.Load())
	}
}
