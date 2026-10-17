package handover

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// CheckHistory accepts unchanged/appended history or an unbroken sequence of
// archived completed generations. Every archive is read by its pinned UID.
// Missing history, forks and recreated objects remain errors; callers retain
// their old checkpoint on failure. This proves history only, not graph safety
// or live participant authorization.
func (s Store) CheckHistory(ctx context.Context, current, previous Snapshot) error {
	if err := s.configured(); err != nil {
		return err
	}
	if current.object == nil || previous.object == nil {
		return fmt.Errorf("history requires both snapshots")
	}
	for _, snapshot := range []Snapshot{current, previous} {
		if _, err := s.snapshot(snapshot.object); err != nil {
			return err
		}
	}
	if err := current.Extends(previous); err == nil {
		return nil
	}
	if current.object.Name != previous.object.Name || current.object.Namespace != previous.object.Namespace || current.object.UID != previous.object.UID {
		return fmt.Errorf("history belongs to a different journal incarnation")
	}
	cursor := current
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		link, found := cursor.Predecessor()
		if !found || !link.valid() || seen[link.ArchiveUID] {
			return fmt.Errorf("history is missing or cycles before the local checkpoint")
		}
		seen[link.ArchiveUID] = true
		reader := Store{ConfigMaps: s.ConfigMaps, Name: link.ArchiveName, ExpectedUID: link.ArchiveUID, ClusterUID: s.ClusterUID}
		archive, err := reader.Load(ctx)
		if err != nil {
			return fmt.Errorf("loading predecessor archive: %w", err)
		}
		// Reconstruct only the archive's source identity for comparison. This
		// synthetic snapshot is never returned as executable or live state.
		source := archive
		source.object = archive.object.DeepCopy()
		source.object.ObjectMeta = metav1.ObjectMeta{
			Name:            archive.object.Annotations["handover.cloud-provisioning.io/source-name"],
			Namespace:       archive.object.Namespace,
			UID:             types.UID(archive.object.Annotations["handover.cloud-provisioning.io/source-uid"]),
			ResourceVersion: archive.object.Annotations["handover.cloud-provisioning.io/source-resource-version"],
		}
		if err := cursor.Succeeds(source, archive); err != nil {
			return err
		}
		if err := source.Extends(previous); err == nil {
			break
		}
		cursor = source
	}
	// Archive reads can span a concurrent live generation change. Do not let
	// successful ancestry verification authorize an obsolete admission.
	live, err := s.Load(ctx)
	if err != nil {
		return err
	}
	return live.ContinuesExecution(current)
}
