package main

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// Pin released histories independently of serving reservations. This is only a
// mutation guard: checkpoint admission still authenticates finalization and the
// durable local fence before native work. Released histories never grant routes.
func loadReleasedReceiveJournals(ctx context.Context, store handover.Store, inventory tunnel.ReceiveInventory) (map[string]handover.Snapshot, error) {
	result := map[string]handover.Snapshot{}
	for uid, publication := range inventory.Released {
		member := store
		member.Name, member.ExpectedUID = tunnel.ReceiveTransitionName(uid), publication.JournalUID
		snapshot, err := member.Load(ctx)
		if err != nil {
			return nil, err
		}
		result[uid] = snapshot
	}
	return result, ctx.Err()
}

func recheckReleasedReceiveJournals(ctx context.Context, store handover.Store, admitted map[string]handover.Snapshot) error {
	for uid, before := range admitted {
		member := store
		member.Name, member.ExpectedUID = tunnel.ReceiveTransitionName(uid), string(before.Metadata().UID)
		if err := member.RecheckExecution(ctx, before); err != nil {
			return fmt.Errorf("released journal changed during execution: %w", err)
		}
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
