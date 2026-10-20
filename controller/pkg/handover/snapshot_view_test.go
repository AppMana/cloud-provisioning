package handover

import (
	"context"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSnapshotViewMatchesHistoryAndDoesNotExposeState(t *testing.T) {
	initial := transitionFixture(t)
	at := epoch.Add(time.Second)
	observed, err := initial.Observe(at, receipts(t, view(t, initial).Round, at)[:1])
	if err != nil {
		t.Fatal(err)
	}
	switched := step(t, observed, at.Add(time.Second))
	drained := step(t, switched, at.Add(2*time.Second))
	retired := step(t, drained, view(t, drained).DrainUntil)
	complete := step(t, retired, view(t, drained).DrainUntil.Add(time.Second))
	rolled, err := switched.Rollback(at.Add(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := initial.Renew(view(t, initial).Round.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	for name, tr := range map[string]Transition{
		"initial": initial, "observed": observed, "switch": switched,
		"drain": drained, "retire": retired, "complete": complete,
		"rollback": rolled, "renewed": renewed,
	} {
		t.Run(name, func(t *testing.T) {
			_, snapshot := checkpointFixture(t, tr)
			want := view(t, tr)
			got, err := snapshot.View()
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("snapshot view differs from replay: %v", err)
			}
			got.Round.Members[0].NodeUID = "changed"
			if len(got.Observed) > 0 {
				got.Observed[0].NodeUID = "changed"
			}
			got.Complete = !got.Complete
			got.Round.ExpiresAt = got.Round.ExpiresAt.Add(time.Hour)
			again, err := snapshot.View()
			if err != nil || !reflect.DeepEqual(again, want) ||
				!reflect.DeepEqual(view(t, snapshot.Transition()), want) {
				t.Fatalf("caller changed immutable snapshot: %v", err)
			}
		})
	}
	if _, err := (Snapshot{}).View(); err == nil {
		t.Fatal("empty snapshot admitted a view")
	}
}

func TestSnapshotViewDoesNotCacheSubsequentLoads(t *testing.T) {
	original := transitionFixture(t)
	base, before := checkpointFixture(t, original)
	for _, kind := range []string{"advance", "corrupt", "recreated", "wrong-cluster"} {
		t.Run(kind, func(t *testing.T) {
			api := fake.NewClientset(before.object.DeepCopy())
			store := base
			store.ConfigMaps = api.CoreV1().ConfigMaps(before.object.Namespace)
			store.ExpectedUID = string(before.object.UID)
			prior, err := store.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := prior.View(); err != nil {
				t.Fatal(err)
			}
			changed := before.object.DeepCopy()
			switch kind {
			case "advance":
				raw, err := step(t, original, epoch.Add(time.Second)).Bytes()
				if err != nil {
					t.Fatal(err)
				}
				changed.Data[journalKey] = string(raw)
			case "corrupt":
				changed.Data[journalKey] = "{"
			case "recreated":
				changed.UID = "replacement"
			case "wrong-cluster":
				store.ClusterUID = "another-cluster"
			}
			// Same resource version deliberately: byte validation cannot rely
			// on the API revision or any previously validated view alone.
			if err := api.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), changed, changed.Namespace); err != nil {
				t.Fatal(err)
			}
			next, err := store.Load(context.Background())
			if kind == "advance" {
				if err != nil {
					t.Fatal(err)
				}
				got, err := next.View()
				if err != nil || got.Round.Phase != Switch || next.ContinuesExecution(prior) == nil {
					t.Fatalf("fresh load reused earlier authority: %v", err)
				}
			} else if err == nil {
				t.Fatal("fresh load accepted invalid live journal")
			}
		})
	}
}

func BenchmarkSnapshotView(b *testing.B) {
	tr := transitionFixture(b)
	for i := range 100 {
		at := epoch.Add(time.Duration(i+1) * time.Millisecond)
		var err error
		tr, err = tr.Observe(at, receipts(b, view(b, tr).Round, at))
		if err != nil {
			b.Fatal(err)
		}
	}
	_, snapshot := checkpointFixture(b, tr)
	b.Run("replay", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := snapshot.Transition().View(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("validated-snapshot", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := snapshot.View(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
