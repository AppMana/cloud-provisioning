package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.org/x/sync/errgroup"
)

// Pin released histories independently of serving reservations. This is only a
// mutation guard: checkpoint admission still authenticates finalization and the
// durable local fence before native work. Released histories never grant routes.
func loadReleasedReceiveJournals(ctx context.Context, store handover.Store, inventory tunnel.ReceiveInventory) (map[string]handover.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := map[string]handover.Snapshot{}
	var mu sync.Mutex
	// Independent named reads retain Store.Load's complete history and
	// incarnation validation. Bound API pressure and join every read before
	// returning; a partial set never authorizes an operation.
	group, readCtx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for uid, publication := range inventory.Released {
		member := store
		member.Name, member.ExpectedUID = tunnel.ReceiveTransitionName(uid), publication.JournalUID
		group.Go(func() error {
			if err := readCtx.Err(); err != nil {
				return err
			}
			snapshot, err := member.Load(readCtx)
			if err != nil {
				return err
			}
			mu.Lock()
			result[uid] = snapshot
			mu.Unlock()
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func recheckReleasedReceiveJournals(ctx context.Context, store handover.Store, admitted map[string]handover.Snapshot) error {
	snapshots := make([]handover.Snapshot, 0, len(admitted))
	for uid, before := range admitted {
		if before.Metadata().Name != tunnel.ReceiveTransitionName(uid) {
			return fmt.Errorf("released journal differs from admitted machine")
		}
		snapshots = append(snapshots, before)
	}
	if err := store.RecheckExecutions(ctx, snapshots); err != nil {
		return fmt.Errorf("released journal changed during execution: %w", err)
	}
	return ctx.Err()
}

func sameReleasedReceiveJournals(a, b map[string]handover.Snapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for uid, before := range a {
		current, found := b[uid]
		if !found || current.ContinuesExecution(before) != nil {
			return false
		}
	}
	return true
}
