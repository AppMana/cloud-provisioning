package handover

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ArchiveCompleted preserves the exact terminal state before a separate
// ownership-generation protocol can replace it. It neither releases ownership
// nor changes discovery or the live journal. The returned archive UID must be
// bound by that protocol; a name alone does not protect against recreation.
// Archives deliberately have no owner reference to a replaceable journal.
func (s Store) ArchiveCompleted(ctx context.Context, previous Snapshot) (Snapshot, error) {
	fail := func(err error) (Snapshot, error) { return Snapshot{}, err }
	if err := s.configured(); err != nil {
		return fail(err)
	}
	if previous.object == nil || previous.object.DeletionTimestamp != nil {
		return fail(fmt.Errorf("archive requires a live journal snapshot"))
	}
	checked, err := s.snapshot(previous.object)
	if err != nil {
		return fail(err)
	}
	view, err := checked.Transition().View()
	if err != nil {
		return fail(err)
	}
	if !view.Complete || view.ServingGeneration == "" {
		return fail(fmt.Errorf("archive requires verified transition completion"))
	}
	current, err := s.Load(ctx)
	if err != nil {
		return fail(err)
	}
	if current.object.DeletionTimestamp != nil || !reflect.DeepEqual(current.object, previous.object) {
		return fail(fmt.Errorf("journal changed before archival"))
	}
	identity := sha256.Sum256([]byte(string(previous.object.UID) + ":" + journalDigest(previous)))
	archive := previous.object.DeepCopy()
	archive.ObjectMeta = metav1.ObjectMeta{
		Name:      fmt.Sprintf("handover-archive-%x", identity[:16]),
		Namespace: previous.object.Namespace,
		Annotations: map[string]string{
			"handover.cloud-provisioning.io/source-name":             previous.object.Name,
			"handover.cloud-provisioning.io/source-uid":              string(previous.object.UID),
			"handover.cloud-provisioning.io/source-resource-version": previous.object.ResourceVersion,
		},
	}
	immutable := true
	archive.Immutable = &immutable
	stored, err := s.ConfigMaps.Create(ctx, archive, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		stored, err = s.ConfigMaps.Get(ctx, archive.Name, metav1.GetOptions{})
	}
	if err != nil {
		return fail(err)
	}
	// Metadata-only source updates do not change the terminal history. Keep
	// the provenance of the first exact archive instead of making that archive
	// permanently block a later compare-and-swap attempt.
	expectedAnnotations := archive.Annotations
	const sourceVersion = "handover.cloud-provisioning.io/source-resource-version"
	if !token(stored.Annotations[sourceVersion]) {
		return fail(fmt.Errorf("archive source version is missing"))
	}
	expectedAnnotations[sourceVersion] = stored.Annotations[sourceVersion]
	if stored.DeletionTimestamp != nil || stored.Immutable == nil || !*stored.Immutable ||
		stored.Namespace != archive.Namespace || len(stored.OwnerReferences) != 0 ||
		!reflect.DeepEqual(stored.Annotations, expectedAnnotations) ||
		!reflect.DeepEqual(stored.Data, archive.Data) || !reflect.DeepEqual(stored.BinaryData, archive.BinaryData) {
		return fail(fmt.Errorf("archive differs from the completed journal"))
	}
	// A concurrent replacement must not be interpreted as successful handoff.
	// A retained archive on failure is harmless and remains available to audit.
	current, err = s.Load(ctx)
	if err != nil {
		return fail(err)
	}
	if current.object.DeletionTimestamp != nil || !reflect.DeepEqual(current.object, previous.object) {
		return fail(fmt.Errorf("journal changed during archival"))
	}
	reader := Store{ConfigMaps: s.ConfigMaps, Name: archive.Name, ClusterUID: s.ClusterUID}
	return reader.snapshot(stored)
}
