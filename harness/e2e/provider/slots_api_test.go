package provider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

type slotTestAPI struct{}

func (slotTestAPI) Run(ctx context.Context, args ...string) ([]byte, error) {
	argv := append([]string{"exec", "clab-cldt-bastion", "kubectl", "--server=https://10.10.0.10:6443"}, args...)
	return exec.CommandContext(ctx, "docker", argv...).Output()
}

type slotConcurrentWriter struct {
	SlotCommands
	beforePatch func()
	fired       bool
}

func (w *slotConcurrentWriter) Run(ctx context.Context, args ...string) ([]byte, error) {
	for _, arg := range args {
		if arg == "patch" && !w.fired {
			w.fired = true
			w.beforePatch()
			break
		}
	}
	return w.SlotCommands.Run(ctx, args...)
}

// Opt-in: uses the retained VM cluster API via its bastion. It creates only a
// dedicated temporary namespace and ConfigMap; it never provisions guests.
func TestSlotStoreRealAPI(t *testing.T) {
	if os.Getenv("CLDT_SLOT_API_TEST") != "1" {
		t.Skip("set CLDT_SLOT_API_TEST=1 for the retained VM API")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	api := slotTestAPI{}
	ns := fmt.Sprintf("cldt-slot-test-%d", time.Now().UnixNano())
	if _, err := api.Run(ctx, "create", "namespace", ns); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := api.Run(cleanup, "delete", "namespace", ns, "--wait=false"); err != nil {
			t.Error(err)
		}
	}()
	store := SlotStore{API: api, Namespace: ns, Name: "pool", Lab: "isolated-test", Slots: []string{"remote3", "remote1", "remote2"}}
	first := SlotOwner{Namespace: ns, Name: "generated-group-0", UID: "infra-0"}
	if slot, err := store.Reserve(ctx, first, ""); err != nil || slot != "remote1" {
		t.Fatal("first allocation", slot, err)
	}
	restarted := SlotStore{API: api, Namespace: ns, Name: "pool", Lab: "isolated-test", Slots: []string{"remote1", "remote2", "remote3"}}
	if slot, err := restarted.Reserve(ctx, first, ""); err != nil || slot != "remote1" {
		t.Fatal("restart moved allocation", slot, err)
	}
	if _, err := store.Reserve(ctx, SlotOwner{Namespace: ns, Name: first.Name, UID: "replacement"}, ""); err == nil {
		t.Fatal("replacement stole old allocation")
	}
	if _, err := store.Reserve(ctx, first, "remote2"); err == nil {
		t.Fatal("owner moved between slots")
	}
	second := SlotOwner{Namespace: ns, Name: "generated-group-1", UID: "infra-1"}
	third := SlotOwner{Namespace: ns, Name: "generated-group-2", UID: "infra-2"}
	racer := &slotConcurrentWriter{SlotCommands: api, beforePatch: func() {
		if slot, err := restarted.Reserve(ctx, second, ""); err != nil || slot != "remote2" {
			t.Fatal("concurrent allocation", slot, err)
		}
	}}
	stale := store
	stale.API = racer
	if _, err := stale.Reserve(ctx, third, ""); err == nil {
		t.Fatal("stale pool update overwrote concurrent owner")
	}
	if slot, err := restarted.Reserve(ctx, third, ""); err != nil || slot != "remote3" {
		t.Fatal("retry reused occupied slot", slot, err)
	}
	if _, err := store.Reserve(ctx, SlotOwner{Namespace: ns, Name: "overflow", UID: "infra-3"}, ""); err == nil {
		t.Fatal("overcommitted pool")
	}
	for _, owner := range []SlotOwner{first, second, third} {
		if _, err := store.Reserve(ctx, owner, ""); err != nil {
			t.Fatal("lost retained owner", err)
		}
	}
	verifyPooledBindings(t, ctx, ns)
	changed := store
	changed.Slots = []string{"remote1", "remote2"}
	if _, err := changed.Reserve(ctx, first, ""); err == nil {
		t.Fatal("pool silently resized")
	}
}
