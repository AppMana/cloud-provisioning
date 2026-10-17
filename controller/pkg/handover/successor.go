package handover

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Predecessor binds a new generation to immutable terminal evidence. It is not
// a receipt and does not authorize native work without graph and live admission.
type Predecessor struct {
	ArchiveName       string `json:"archiveName"`
	ArchiveUID        string `json:"archiveUID"`
	SourceUID         string `json:"sourceUID"`
	JournalSHA256     string `json:"journalSHA256"`
	ServingGeneration string `json:"servingGeneration"`
}

func (p Predecessor) valid() bool {
	return token(p.ArchiveName) && token(p.ArchiveUID) && token(p.SourceUID) && hexToken(p.JournalSHA256, 32) && token(p.ServingGeneration)
}

func journalDigest(s Snapshot) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s.object.Data[journalKey])))
}

// Predecessor returns a copy of the immutable archive reference. Callers must
// load that exact UID from trusted storage and use Succeeds before admitting a
// different generation over an existing durable checkpoint.
func (s Snapshot) Predecessor() (Predecessor, bool) {
	if s.transition.j.Predecessor == nil {
		return Predecessor{}, false
	}
	return *s.transition.j.Predecessor, true
}

// Succeeds verifies one generation boundary, including a checkpoint that
// predates terminal receipts. It does not authenticate live participants or
// authorize native mutation. Skipped generations require their entire chain;
// a newer journal cannot simply replace an unrelated local checkpoint.
func (s Snapshot) Succeeds(previous, archive Snapshot) error {
	if s.object == nil || previous.object == nil || archive.object == nil {
		return fmt.Errorf("successor admission requires complete snapshots")
	}
	p, found := s.Predecessor()
	if !found || !p.valid() {
		return fmt.Errorf("successor has no predecessor binding")
	}
	for _, snapshot := range []Snapshot{s, previous, archive} {
		if snapshot.object.UID == "" || snapshot.object.ResourceVersion == "" || snapshot.object.DeletionTimestamp != nil {
			return fmt.Errorf("successor admission requires live identities")
		}
		if _, err := snapshot.Transition().View(); err != nil {
			return err
		}
	}
	if s.object.Name != previous.object.Name || s.object.Namespace != previous.object.Namespace || s.object.UID != previous.object.UID || p.SourceUID != string(previous.object.UID) ||
		archive.object.Name != p.ArchiveName || string(archive.object.UID) != p.ArchiveUID || archive.object.Namespace != s.object.Namespace || archive.object.Immutable == nil || !*archive.object.Immutable || len(archive.object.OwnerReferences) != 0 ||
		archive.object.Annotations["handover.cloud-provisioning.io/source-name"] != previous.object.Name || archive.object.Annotations["handover.cloud-provisioning.io/source-uid"] != p.SourceUID ||
		journalDigest(archive) != p.JournalSHA256 || !bytes.Equal(archive.Plan(), previous.Plan()) {
		return fmt.Errorf("successor archive identity or contents differ")
	}
	if err := archive.Transition().Extends(previous.Transition()); err != nil {
		return err
	}
	old, _ := archive.Transition().View()
	initial := s.transition.j.Initial
	if !old.Complete || old.ServingGeneration != p.ServingGeneration || initial.ClusterUID != old.Round.ClusterUID || initial.FromGeneration != old.ServingGeneration || initial.TransitionUID == old.Round.TransitionUID || initial.PlanHash == old.Round.PlanHash {
		return fmt.Errorf("successor does not continue archived completion")
	}
	if initial.IssuedAt.Before(archive.transition.j.Events[len(archive.transition.j.Events)-1].At) {
		return fmt.Errorf("successor predates archived completion")
	}
	return continuesMembers(old, initial)
}

// ReplaceCompleted atomically starts a new generation in the same journal
// incarnation after preserving its terminal predecessor. Callers must validate
// the complete public graph and coordinate discovery and native ownership;
// this storage operation alone does not authorize either. Version two makes
// older transition readers fail closed rather than forget the predecessor.
func (s Store) ReplaceCompleted(ctx context.Context, previous Snapshot, spec Spec, plan []byte, now time.Time, policy Policy) (Snapshot, error) {
	fail := func(err error) (Snapshot, error) { return Snapshot{}, err }
	if previous.object == nil || len(plan) == 0 {
		return fail(fmt.Errorf("successor requires a predecessor and public plan"))
	}
	checked, err := s.snapshot(previous.object)
	if err != nil {
		return fail(err)
	}
	old, err := checked.Transition().View()
	if err != nil {
		return fail(err)
	}
	if !old.Complete || spec.ClusterUID != old.Round.ClusterUID || spec.FromGeneration != old.ServingGeneration || spec.TransitionUID == old.Round.TransitionUID || spec.PlanHash == old.Round.PlanHash {
		return fail(fmt.Errorf("successor does not continue the completed generation"))
	}
	if spec.PlanHash != fmt.Sprintf("%x", sha256.Sum256(plan)) {
		return fail(fmt.Errorf("successor public plan hash differs"))
	}
	last := checked.transition.j.Events[len(checked.transition.j.Events)-1].At
	if now.Before(last) {
		return fail(fmt.Errorf("successor predates predecessor completion"))
	}
	next, err := NewTransition(spec, now, policy)
	if err != nil {
		return fail(err)
	}
	if err := continuesMembers(old, next.j.Initial); err != nil {
		return fail(err)
	}
	archive, err := s.ArchiveCompleted(ctx, checked)
	if err != nil {
		return fail(err)
	}
	next.j.Version, next.j.PlanRequired = 2, true
	next.j.Predecessor = &Predecessor{ArchiveName: archive.Metadata().Name, ArchiveUID: string(archive.Metadata().UID), SourceUID: string(previous.object.UID), JournalSHA256: journalDigest(checked), ServingGeneration: old.ServingGeneration}
	raw, err := next.Bytes()
	if err != nil {
		return fail(err)
	}
	object := previous.object.DeepCopy()
	object.Data[journalKey], object.Data[planKey] = string(raw), string(plan)
	updated, err := s.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{})
	if err != nil {
		return fail(err)
	}
	if updated.UID != previous.object.UID || !reflect.DeepEqual(updated.Data, object.Data) {
		return fail(fmt.Errorf("persisted successor differs"))
	}
	return s.snapshot(updated)
}

// Every old participant must carry its actually serving local and peer keys
// into preparation. Membership changes need a separate retirement/admission
// protocol; silently dropping a member would lose native ownership evidence.
func continuesMembers(old View, next Round) error {
	if len(old.Round.Members) != len(next.Members) {
		return fmt.Errorf("successor changes participant set")
	}
	for i, previous := range old.Round.Members {
		current := next.Members[i]
		local, peer := previous.ToKey, previous.ToPeerKey
		if old.Round.Phase == Abort {
			local, peer = previous.FromKey, previous.FromPeerKey
		}
		if current.NodeUID != previous.NodeUID || current.DeviceID != previous.DeviceID || current.FromKey != local || current.FromPeerKey != peer {
			return fmt.Errorf("successor changes serving participant identity")
		}
	}
	return nil
}
