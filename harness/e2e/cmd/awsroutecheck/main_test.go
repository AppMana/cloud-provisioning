package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	provider "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
)

func TestFileJournalRejectsStaleLeaseRemoval(t *testing.T) {
	ctx := context.Background()
	j := journal{filepath.Join(t.TempDir(), "route.json")}
	initial := &provider.RouteRecord{Key: "owned-route", Phase: "Creating", Leases: map[string]bool{"first": true}}
	if err := j.Save(ctx, nil, initial); err != nil {
		t.Fatal(err)
	}
	stale, err := j.Load(ctx, initial.Key)
	if err != nil {
		t.Fatal(err)
	}
	newer := *stale
	newer.Phase = "Active"
	newer.Leases = map[string]bool{"first": true, "second": true}
	if err := j.Save(ctx, stale, &newer); err != nil {
		t.Fatal(err)
	}
	removal := *stale
	removal.Phase = "Deleting"
	removal.Leases = nil
	if err := j.Save(ctx, stale, &removal); err == nil {
		t.Fatal("stale release erased second lease")
	}
	current, err := j.Load(ctx, initial.Key)
	if err != nil || !current.Leases["second"] {
		t.Fatal("shared lease lost")
	}
	if _, err := j.Load(ctx, "foreign-route"); err == nil {
		t.Fatal("journal accepted another route identity")
	}
}

func TestRecoveryRejectsChangedScopeBeforeAwsCalls(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"scope": provider.Scope{RouteTableID: "foreign"}, "leases": []string{"a", "b"}})
	if err := os.WriteFile(filepath.Join(dir, "inputs.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := recoverRoutes(context.Background(), options{work: dir, output: dir}, provider.Scope{RouteTableID: "owned"}); err == nil {
		t.Fatal("foreign recovery scope accepted")
	}
}

func TestCheckRejectsSubnetRouteBeforeAnyObservation(t *testing.T) {
	if err := run(options{work: t.TempDir(), output: "unused", plans: "unused", apiServer: "https://unused", destination: "10.10.0.0/24"}); err == nil {
		t.Fatal("subnet route accepted")
	}
}
