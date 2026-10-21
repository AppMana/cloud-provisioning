package main

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

func TestReceiveDispatchServicesExpiringRoundBeforeMaintenance(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rounds := map[string]handover.View{
		"a-completed": {Complete: true},
		"b-later":     {Round: handover.Round{ExpiresAt: now.Add(5 * time.Minute)}},
		"z-earlier":   {Round: handover.Round{ExpiresAt: now.Add(30 * time.Second)}},
	}
	before := map[string]handover.View{}
	for uid, view := range rounds {
		before[uid] = view
	}
	targets := []string{"a-completed", "b-later", "z-earlier"}
	orderReceiveDispatchTargets(targets, nil, rounds)
	// Deterministic work accounting: maintenance plus the newer round would
	// consume the older round's remaining window. This is a scheduler test,
	// not evidence of real API latency or authenticated receipt completion.
	elapsed := time.Duration(0)
	seen := map[string]bool{}
	for _, uid := range targets {
		if seen[uid] {
			t.Fatal("target executed twice")
		}
		seen[uid] = true
		elapsed += 20 * time.Second
		if !rounds[uid].Complete && !now.Add(elapsed).Before(rounds[uid].Round.ExpiresAt) {
			t.Fatalf("dispatch postponed %s beyond its remaining round lifetime", uid)
		}
	}
	if len(seen) != len(rounds) || targets[len(targets)-1] != "a-completed" {
		t.Fatal("completed maintenance must remain scheduled after active rounds")
	}
	if !reflect.DeepEqual(rounds, before) {
		t.Fatal("scheduling changed admitted views or extended their deadlines")
	}
}

func TestReceiveDispatchKeepsWithdrawalsTogetherAndTiesStable(t *testing.T) {
	deadline := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	targets := []string{"complete-a", "active-b", "withdraw-a", "active-a", "complete-b", "withdraw-b"}
	rounds := map[string]handover.View{
		"complete-a": {Complete: true}, "complete-b": {Complete: true},
		"active-a":   {Round: handover.Round{ExpiresAt: deadline}},
		"active-b":   {Round: handover.Round{ExpiresAt: deadline}},
		"withdraw-a": {Complete: true},
		"withdraw-b": {Round: handover.Round{ExpiresAt: deadline.Add(-time.Minute)}},
	}
	orderReceiveDispatchTargets(targets, map[string]bool{"withdraw-a": true, "withdraw-b": true}, rounds)
	want := []string{"withdraw-a", "withdraw-b", "active-b", "active-a", "complete-a", "complete-b"}
	if !slices.Equal(targets, want) {
		t.Fatalf("withdrawal cohort, discovery-order ties or maintenance lost: got %v want %v", targets, want)
	}
}
