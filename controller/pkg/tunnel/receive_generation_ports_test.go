package tunnel

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Serialization and allocation coverage only. API CAS, publication ordering,
// native coexistence and acknowledged listener retirement are separate gates.
func TestReceiveGenerationReservationsSurviveRestart(t *testing.T) {
	peer := receiverTestKey(8)
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)
	r := ReceivePortReservations{Version: 1, MeshUID: "mesh", NodeUID: "node", RootPublicKey: receiverTestKey(24), Ports: map[string]int{peer: 51821}}
	before, _ := json.Marshal(r)
	paths := []ReceivePath{{PublicKey: peer}}
	assigned, updated, err := r.Assign(first, paths, []int{51821, 51822, 51823})
	if err != nil || assigned[peer] != 51822 || updated.Version != 2 {
		t.Fatalf("successor allocation: %v %v", assigned, err)
	}
	raw, _ := json.Marshal(updated)
	var restarted ReceivePortReservations
	if err := json.Unmarshal(raw, &restarted); err != nil {
		t.Fatal(err)
	}
	retry, retained, err := restarted.Assign(first, paths, []int{51823, 51822, 51821})
	if err != nil || !reflect.DeepEqual(retry, assigned) || !reflect.DeepEqual(retained, restarted) {
		t.Fatalf("restart changed reservations: %v", err)
	}
	next, all, err := retained.Assign(second, paths, []int{51821, 51822, 51823})
	if err != nil || next[peer] != 51823 || all.Ports[peer] != 51821 || all.Generations[first][peer] != 51822 {
		t.Fatalf("later generation reused a listener: %v", err)
	}
	if _, _, err := all.Assign(strings.Repeat("c", 64), paths, []int{51821, 51822, 51823}); err == nil {
		t.Fatal("exhaustion reused retained generation")
	}
	legacy, again, err := all.Assign("", paths, []int{51821, 51822, 51823})
	if err != nil || legacy[peer] != 51821 || !reflect.DeepEqual(again, all) {
		t.Fatal("legacy reconciliation lost successor reservations", err)
	}
	all.Generations[first][peer] = 60000
	if retained.Generations[first][peer] != 51822 {
		t.Fatal("candidate aliases previous reservations")
	}
	after, _ := json.Marshal(r)
	if string(before) != string(after) {
		t.Fatal("assignment mutated predecessor")
	}
	for _, fault := range []string{"downgrade", "duplicate", "invalid-generation", "missing-map"} {
		var broken ReceivePortReservations
		if err := json.Unmarshal(raw, &broken); err != nil {
			t.Fatal(err)
		}
		switch fault {
		case "downgrade":
			broken.Version = 1
		case "duplicate":
			broken.Generations[first][peer] = 51821
		case "invalid-generation":
			broken.Generations["bad"] = map[string]int{}
		case "missing-map":
			broken.Generations[first] = nil
		}
		if _, _, err := broken.Assign(first, paths, []int{51821, 51822, 51823}); err == nil {
			t.Fatalf("accepted %s reservation state", fault)
		}
	}
}
