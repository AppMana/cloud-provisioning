package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// Caller holds the host lock and has checkpointed live withdrawal authority.
// recorded supplies separately admitted journal/legacy-cache permissions. This
// operation uses the existing ordinary-peer sidecar, never a parallel journal.
// It does not retire receiver devices or forwarding caches, or issue a receipt.
func retireWithdrawnNeighborCaches(ctx context.Context, path string, current receiveNeighborCheckpoint, plans []tunnel.ReceiveTransitionPlan, root string, recorded []tunnel.PeerSpec, check func() error, retire func([]tunnel.PeerSpec) error) error {
	if path == "" || current.Version != 1 || current.InventoryUID == "" || current.MeshUID == "" || current.NodeUID == "" || current.BootID == "" || current.PublicKey == "" || current.Peers == nil || check == nil || retire == nil {
		return fmt.Errorf("neighbor withdrawal requires admitted identity and native guard")
	}
	type cached struct {
		path  string
		raw   []byte
		value receiveNeighborCheckpoint
	}
	var caches []cached
	previous := slices.Clone(recorded)
	intent := current
	intent.Retiring, intent.Candidates = nil, nil
	for _, source := range []string{path, path + ".retiring"} {
		info, err := os.Lstat(source)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return fmt.Errorf("neighbor checkpoint must be a private regular file")
		}
		raw, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		value, err := decodeReceiveNeighborCheckpoint(raw, current, source == path)
		if err != nil {
			return err
		}
		caches = append(caches, cached{source, raw, value})
		previous = append(previous, value.Peers...)
		previous = append(previous, value.Retiring...)
		previous = append(previous, value.Candidates...)
		if source != path {
			intent = value
		}
	}
	selected, err := tunnel.PlanReceiveWithdrawnNeighborPeers(previous, current.Peers, plans, root)
	if err != nil {
		return err
	}
	// Keep all exact native possibilities before touching the kernel, including
	// records imported from an old executable cache or committed journal graph.
	known := map[string]bool{}
	for _, peer := range intent.Retiring {
		known[neighborPermissionIdentity(peer)] = true
	}
	for _, peer := range selected {
		if id := neighborPermissionIdentity(peer); !known[id] {
			intent.Retiring = append(intent.Retiring, peer)
			known[id] = true
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	if err := writeDurableJSON(path+".retiring", intent); err != nil {
		return err
	}
	// Replace the sidecar snapshot with the durable intent just written, so a
	// concurrent or unexpected rewrite cannot be mistaken for our completion.
	raw, err := os.ReadFile(path + ".retiring")
	if err != nil {
		return err
	}
	found := false
	for i := range caches {
		if caches[i].path == path+".retiring" {
			caches[i] = cached{path + ".retiring", raw, intent}
			found = true
		}
	}
	if !found {
		caches = append(caches, cached{path + ".retiring", raw, intent})
	}
	if err := check(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := retire(selected); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	// Validate every input before removing any executable reference. Partial
	// durable writes remain retryable by reloading; no atomic multi-file claim.
	for _, cache := range caches {
		info, err := os.Lstat(cache.path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return fmt.Errorf("neighbor checkpoint ownership changed")
		}
		raw, err := os.ReadFile(cache.path)
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, cache.raw) {
			return fmt.Errorf("neighbor checkpoint changed during withdrawal")
		}
	}
	without := func(peers []tunnel.PeerSpec) []tunnel.PeerSpec {
		if peers == nil {
			return nil
		}
		next := make([]tunnel.PeerSpec, 0, len(peers))
		for _, peer := range peers {
			if peer.PublicKey != root {
				next = append(next, peer)
			}
		}
		return next
	}
	// Keep the retirement sidecar until executable main-cache removal commits.
	// A failure after either write leaves sufficient evidence for the next retry.
	for _, cache := range caches {
		next := cache.value
		next.Peers, next.Retiring, next.Candidates = without(next.Peers), without(next.Retiring), without(next.Candidates)
		if err := writeDurableJSON(cache.path, next); err != nil {
			return err
		}
	}
	return nil
}
