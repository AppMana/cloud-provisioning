package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func verifySlotRelease(t *testing.T, ctx context.Context, s SlotStore) {
	t.Helper()
	name := "aaa-generated-0"
	raw, err := s.API.Run(ctx, "-n", s.Namespace, "get", machineKind, name, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var object struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	owner := SlotOwner{Namespace: s.Namespace, Name: name, UID: object.Metadata.UID}
	stops := 0
	stop := func() error { stops++; return nil }
	if err := s.Stop(ctx, "remote2", owner, stop); err == nil || stops != 0 {
		t.Fatal("active owner stopped", err, stops)
	}
	if _, err := s.API.Run(ctx, "-n", s.Namespace, "patch", machineKind, name, "--type=merge", "-p", `{"metadata":{"finalizers":["test/hold"]}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.API.Run(ctx, "-n", s.Namespace, "delete", machineKind, name, "--wait=false"); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, "remote2", owner, func() error { return fmt.Errorf("native stop failed") }); err == nil {
		t.Fatal("failed stop accepted")
	}
	if n, err := s.ReleaseStopped(ctx); err != nil || n != 0 {
		t.Fatal("unstopped slot released", n, err)
	}
	if err := s.Stop(ctx, "remote2", owner, stop); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, "remote2", owner, stop); err != nil || stops != 1 {
		t.Fatal("recorded stop repeated", err, stops)
	}
	if n, err := s.ReleaseStopped(ctx); err != nil || n != 0 {
		t.Fatal("terminating owner released", n, err)
	}
	if _, err := s.API.Run(ctx, "-n", s.Namespace, "patch", machineKind, name, "--type=merge", "-p", `{"metadata":{"finalizers":[]}}`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ReleaseStopped(ctx); err != nil || n != 1 {
		t.Fatal("absent stopped owner not released", n, err)
	}
	if n, err := s.ReleaseStopped(ctx); err != nil || n != 0 {
		t.Fatal("release not repeatable", n, err)
	}
	replacement := SlotOwner{Namespace: s.Namespace, Name: name, UID: "replacement-infrastructure-uid"}
	if slot, err := s.Reserve(ctx, replacement, ""); err != nil || slot != "remote2" {
		t.Fatal("replacement could not reuse released slot", slot, err)
	}
	if err := s.Stop(ctx, "remote2", owner, stop); err == nil || stops != 1 {
		t.Fatal("stale stop touched replacement", err, stops)
	}
}
