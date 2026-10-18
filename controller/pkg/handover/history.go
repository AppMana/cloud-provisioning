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
		_, source, err := s.readPredecessor(ctx, cursor)
		if err != nil {
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

// ArchiveNames returns only the verified, pinned predecessor chain needed by
// readers of this journal. It grants no access and never scans the namespace.
func (s Store) ArchiveNames(ctx context.Context, current Snapshot) ([]string, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	if current.object == nil {
		return nil, fmt.Errorf("missing current journal")
	}
	if _, err := s.snapshot(current.object); err != nil {
		return nil, err
	}
	var names []string
	seen := map[string]bool{}
	cursor := current
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		link, found := cursor.Predecessor()
		if !found {
			break
		}
		if !link.valid() || seen[link.ArchiveUID] {
			return nil, fmt.Errorf("invalid or cyclic archive history")
		}
		seen[link.ArchiveUID] = true
		archive, source, err := s.readPredecessor(ctx, cursor)
		if err != nil {
			return nil, err
		}
		names = append(names, archive.Metadata().Name)
		cursor = source
	}
	live, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	if err := live.ContinuesExecution(current); err != nil {
		return nil, err
	}
	return names, nil
}

func (s Store) readPredecessor(ctx context.Context, current Snapshot) (Snapshot, Snapshot, error) {
	fail := func(err error) (Snapshot, Snapshot, error) { return Snapshot{}, Snapshot{}, err }
	link, found := current.Predecessor()
	if !found || !link.valid() {
		return fail(fmt.Errorf("missing predecessor binding"))
	}
	reader := Store{ConfigMaps: s.ConfigMaps, Name: link.ArchiveName, ExpectedUID: link.ArchiveUID, ClusterUID: s.ClusterUID}
	archive, err := reader.Load(ctx)
	if err != nil {
		return fail(fmt.Errorf("loading predecessor archive: %w", err))
	}
	// Reconstruct only the archive's source identity for comparison. This
	// synthetic snapshot is never returned to callers as executable live state.
	source := archive
	source.object = archive.object.DeepCopy()
	source.object.ObjectMeta = metav1.ObjectMeta{
		Name:            archive.object.Annotations["handover.cloud-provisioning.io/source-name"],
		Namespace:       archive.object.Namespace,
		UID:             types.UID(archive.object.Annotations["handover.cloud-provisioning.io/source-uid"]),
		ResourceVersion: archive.object.Annotations["handover.cloud-provisioning.io/source-resource-version"],
	}
	if err := current.Succeeds(source, archive); err != nil {
		return fail(err)
	}
	return archive, source, nil
}

// PredecessorArchive returns the exact immutable immediate predecessor after
// checking its pinned identity, terminal history and the current live journal.
// The archive retains its archive identity; it is never executable live state.
// Call ArchiveNames when the complete ancestry is required.
func (s Store) PredecessorArchive(ctx context.Context, current Snapshot) (Snapshot, error) {
	if err := s.configured(); err != nil {
		return Snapshot{}, err
	}
	if current.object == nil {
		return Snapshot{}, fmt.Errorf("missing successor journal")
	}
	if _, err := s.snapshot(current.object); err != nil {
		return Snapshot{}, err
	}
	archive, _, err := s.readPredecessor(ctx, current)
	if err != nil {
		return Snapshot{}, err
	}
	live, err := s.Load(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if err := live.ContinuesExecution(current); err != nil {
		return Snapshot{}, err
	}
	return archive, nil
}
