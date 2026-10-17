package handover

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

type successorConflictMaps struct {
	typedcore.ConfigMapInterface
	source string
	fired  bool
}

func (m *successorConflictMaps) Update(ctx context.Context, object *corev1.ConfigMap, options metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	if object.Name == m.source && !m.fired {
		m.fired = true
		current, err := m.ConfigMapInterface.Get(ctx, m.source, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations["concurrent-writer"] = "observed"
		if _, err := m.ConfigMapInterface.Update(ctx, current, options); err != nil {
			return nil, err
		}
	}
	return m.ConfigMapInterface.Update(ctx, object, options)
}

// Runs inside the real-API archive cases for forward, abort and rollback.
// Covers storage and participant continuity, not graph semantics, native
// execution, inventory publication or offline recovery across generations.
func verifyStoredSuccessor(t *testing.T, store Store, previous Snapshot) {
	t.Helper()
	ctx := context.Background()
	old := view(t, previous.Transition())
	spec := view(t, transportFixture(t)).Round.Spec
	spec.TransitionUID = "successor-transition"
	spec.FromGeneration, spec.ToGeneration = old.ServingGeneration, "successor-generation"
	plan := []byte(`{"version":2,"transport":"successor-test"}`)
	spec.PlanHash = fmt.Sprintf("%x", sha256.Sum256(plan))
	for i := range spec.Members {
		member := old.Round.Members[i]
		spec.Members[i].FromKey, spec.Members[i].FromPeerKey = member.ToKey, member.ToPeerKey
		if old.Round.Phase == Abort {
			spec.Members[i].FromKey, spec.Members[i].FromPeerKey = member.FromKey, member.FromPeerKey
		}
	}
	at := epoch.Add(30 * time.Second)
	policy := previous.transition.j.Policy
	for _, kind := range []string{"generation", "cluster", "participant", "source-key", "plan", "time"} {
		bad := spec
		bad.Members = append([]Member(nil), spec.Members...)
		now := at
		switch kind {
		case "generation":
			bad.FromGeneration = "wrong"
		case "cluster":
			bad.ClusterUID = "wrong"
		case "participant":
			bad.Members = bad.Members[:len(bad.Members)-1]
		case "source-key":
			bad.Members[0].FromKey = spec.Members[0].ToKey
		case "plan":
			bad.PlanHash = old.Round.PlanHash
		case "time":
			now = epoch
		}
		if _, err := store.ReplaceCompleted(ctx, previous, bad, plan, now, policy); err == nil {
			t.Fatalf("accepted invalid %s successor", kind)
		}
	}
	conflict := &successorConflictMaps{ConfigMapInterface: store.ConfigMaps, source: store.Name}
	racing := store
	racing.ConfigMaps = conflict
	if _, err := racing.ReplaceCompleted(ctx, previous, spec, plan, at, policy); !apierrors.IsConflict(err) || !conflict.fired {
		t.Fatalf("concurrent writer was not fenced: %v", err)
	}
	var err error
	previous, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !view(t, previous.Transition()).Complete {
		t.Fatal("conflicting successor changed the live generation")
	}
	next, err := store.ReplaceCompleted(ctx, previous, spec, plan, at, policy)
	if err != nil {
		t.Fatal(err)
	}
	if next.Metadata().UID != previous.Metadata().UID || next.Metadata().ResourceVersion == previous.Metadata().ResourceVersion {
		t.Fatal("successor did not atomically update the admitted journal")
	}
	if next.transition.j.Version != 2 || next.transition.j.Predecessor == nil {
		t.Fatal("successor lost predecessor binding")
	}
	if err := next.Extends(previous); err == nil {
		t.Fatal("successor bypassed separate generation admission")
	}
	if _, err := store.ReplaceCompleted(ctx, previous, spec, plan, at, policy); err == nil {
		t.Fatal("stale predecessor replaced successor")
	}
	link := next.transition.j.Predecessor
	archiveStore := Store{ConfigMaps: store.ConfigMaps, Name: link.ArchiveName, ClusterUID: store.ClusterUID, ExpectedUID: link.ArchiveUID}
	archived, err := archiveStore.Load(ctx)
	if err != nil || journalDigest(archived) != journalDigest(previous) {
		t.Fatalf("predecessor archive changed: %v", err)
	}
	if err := next.Succeeds(previous, archived); err != nil {
		t.Fatalf("rejected verified generation boundary: %v", err)
	}
	if err := next.Succeeds(previous, previous); err == nil {
		t.Fatal("accepted live source as immutable archive")
	}
	wrong := archived
	wrong.object = archived.object.DeepCopy()
	wrong.object.UID = "recreated-archive"
	if err := next.Succeeds(previous, wrong); err == nil {
		t.Fatal("accepted recreated archive")
	}
	// A host can have missed the final observation rounds, but the archive
	// must extend its exact checkpoint rather than merely share a plan hash.
	earlier := previous
	earlier.transition.j.Events = earlier.transition.j.Events[:0]
	earlier.object = previous.object.DeepCopy()
	raw, err := earlier.transition.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	earlier.object.Data[journalKey] = string(raw)
	if err := next.Succeeds(earlier, archived); err != nil {
		t.Fatalf("rejected earlier checkpoint in archived history: %v", err)
	}
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Extends(next); err != nil {
		t.Fatalf("restart lost successor: %v", err)
	}
	for !view(t, loaded.Transition()).Complete {
		v := view(t, loaded.Transition())
		now := v.Round.IssuedAt.Add(time.Second)
		if now.Before(v.DrainUntil) {
			now = v.DrainUntil
		}
		loaded, err = store.Commit(ctx, loaded, step(t, loaded.Transition(), now))
		if err != nil {
			t.Fatal(err)
		}
	}
	secondArchive, err := store.ArchiveCompleted(ctx, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if secondArchive.Metadata().Name == archived.Metadata().Name {
		t.Fatal("two generations shared an archive name")
	}
}
