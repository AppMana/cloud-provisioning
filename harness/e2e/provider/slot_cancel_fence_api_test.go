package provider

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
)

func createSlotTestOwner(t *testing.T, ctx context.Context, namespace, name string) SlotOwner {
	t.Helper()
	k := &kube.Client{Bastion: slotAPIBastion{}, ControlPlanes: []string{"10.10.0.10"}}
	data, _ := json.Marshal(map[string]any{"apiVersion": "containernet.appmana.com/v1beta2", "kind": "ContainernetMachine", "metadata": map[string]any{"name": name, "namespace": namespace, "finalizers": []string{"test/hold"}}, "spec": map[string]any{}})
	if err := k.Apply(ctx, data); err != nil {
		t.Fatal(err)
	}
	raw, err := k.Run(ctx, "-n", namespace, "get", machineKind, name, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var object struct{ Metadata struct{ UID string } }
	if err := json.Unmarshal(raw, &object); err != nil || object.Metadata.UID == "" {
		t.Fatal("missing fixture UID", err)
	}
	return SlotOwner{Namespace: namespace, Name: name, UID: object.Metadata.UID}
}

// Exercise both orderings against real resource-version conflicts. These are
// infrastructure API fixtures in a private namespace; they never launch VMs.
func verifyCancellationFences(t *testing.T, ctx context.Context, ns string) {
	t.Helper()
	api := slotTestAPI{}
	store := SlotStore{API: api, Namespace: ns, Name: "cancel-fence", Lab: "cldt", Slots: []string{"remote1", "remote2"}}
	anchor := createSlotTestOwner(t, ctx, ns, "fence-anchor")
	if _, err := store.Reserve(ctx, anchor, "remote1"); err != nil {
		t.Fatal(err)
	}
	owner := createSlotTestOwner(t, ctx, ns, "fence-cancel-wins")
	if _, err := store.CancelUnallocated(ctx, owner); err == nil {
		t.Fatal("cancelled an active Machine")
	}
	stale := store
	stale.API = &slotConcurrentWriter{SlotCommands: api, beforePatch: func() {
		if _, err := api.Run(ctx, "-n", ns, "delete", machineKind, owner.Name, "--wait=false"); err != nil {
			t.Fatal(err)
		}
		if cancelled, err := store.CancelUnallocated(ctx, owner); err != nil || !cancelled {
			t.Fatal("cancellation did not fence allocation", cancelled, err)
		}
	}}
	if _, err := stale.Reserve(ctx, owner, "remote2"); err == nil {
		t.Fatal("pre-deletion allocator crossed the cancellation fence")
	}
	if _, err := store.Reserve(ctx, owner, "remote2"); !errors.Is(err, ErrSlotOwnerInactive) {
		t.Fatal("reconstructed allocator accepted deleting Machine", err)
	}
	if cancelled, err := store.CancelUnallocated(ctx, owner); err != nil || !cancelled {
		t.Fatal("reconstructed cancellation failed", cancelled, err)
	}
	_, pool, err := store.readPool(ctx)
	if err != nil || len(pool.Owners) != 1 || pool.Owners["remote1"] != anchor {
		t.Fatal("cancellation changed another reservation", pool, err)
	}

	// If allocation wins before deletion, cancellation must demand teardown.
	winner := createSlotTestOwner(t, ctx, ns, "fence-allocation-wins")
	if _, err := store.Reserve(ctx, winner, "remote2"); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Run(ctx, "-n", ns, "delete", machineKind, winner.Name, "--wait=false"); err != nil {
		t.Fatal(err)
	}
	if cancelled, err := store.CancelUnallocated(ctx, winner); err != nil || cancelled {
		t.Fatal("allocated Machine bypassed teardown", cancelled, err)
	}

	// The first operation may be cancellation before any pool was created.
	empty := store
	empty.Name = "cancel-empty-pool"
	if cancelled, err := empty.CancelUnallocated(ctx, owner); err != nil || !cancelled {
		t.Fatal("empty-pool cancellation failed", cancelled, err)
	}
	if _, err := empty.Reserve(ctx, owner, ""); !errors.Is(err, ErrSlotOwnerInactive) {
		t.Fatal("empty-pool cancellation allowed late allocation", err)
	}
	absent := SlotOwner{Namespace: ns, Name: "missing-owner", UID: "missing-uid"}
	if _, err := empty.Reserve(ctx, absent, ""); !errors.Is(err, ErrSlotOwnerInactive) {
		t.Fatal("absent owner reserved a VM", err)
	}
	replacement := anchor
	replacement.UID = "obsolete-uid"
	if _, err := empty.Reserve(ctx, replacement, ""); !errors.Is(err, ErrSlotOwnerInactive) {
		t.Fatal("obsolete UID reserved a VM", err)
	}
	first := store
	first.Name = "cancel-first-create-race"
	late := createSlotTestOwner(t, ctx, ns, "fence-first-create")
	paused := first
	paused.API = &slotConcurrentWriter{SlotCommands: api, operation: "create", beforePatch: func() {
		if _, err := api.Run(ctx, "-n", ns, "delete", machineKind, late.Name, "--wait=false"); err != nil {
			t.Fatal(err)
		}
		if cancelled, err := first.CancelUnallocated(ctx, late); err != nil || !cancelled {
			t.Fatal("first pool creation did not fence allocation", cancelled, err)
		}
	}}
	if _, err := paused.Reserve(ctx, late, ""); err == nil {
		t.Fatal("stale first allocator crossed cancellation")
	}
	_, pool, err = first.readPool(ctx)
	if err != nil || len(pool.Owners) != 0 {
		t.Fatal("cancelled first allocation left a reservation", pool, err)
	}
}
