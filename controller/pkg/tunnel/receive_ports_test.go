package tunnel

import (
	"reflect"
	"testing"
)

func TestReceivePortReservationsSurviveMembershipChanges(t *testing.T) {
	a, b, c := receiverTestKey(8), receiverTestKey(16), receiverTestKey(24)
	pool := []int{51823, 51821, 51822}
	assigned, reserved, err := AssignReceivePorts([]ReceivePath{{PublicKey: b}, {PublicKey: a}}, map[string]int{}, pool)
	if err != nil {
		t.Fatal(err)
	}
	if assigned[a] == assigned[b] || len(reserved) != 2 {
		t.Fatal("ports were not separately reserved")
	}
	before := map[string]int{a: reserved[a], b: reserved[b]}
	changed, retained, err := AssignReceivePorts([]ReceivePath{{PublicKey: c}, {PublicKey: b}}, reserved, pool)
	if err != nil {
		t.Fatal(err)
	}
	if changed[b] != assigned[b] || changed[c] == assigned[a] || retained[a] != assigned[a] || len(retained) != 3 {
		t.Fatal("surviving or retained receiver port was reassigned")
	}
	if !reflect.DeepEqual(reserved, before) {
		t.Fatal("port planning mutated prior reservations")
	}
	if _, _, err := AssignReceivePorts([]ReceivePath{{PublicKey: receiverTestKey(32)}}, retained, pool); err == nil {
		t.Fatal("exhaustion reused a retained port")
	}
	again, _, err := AssignReceivePorts([]ReceivePath{{PublicKey: a}, {PublicKey: b}}, reserved, []int{51822, 51821, 51823})
	if err != nil || !reflect.DeepEqual(assigned, again) {
		t.Fatal("ordering changed existing assignments")
	}
}

func TestReceivePortsRejectUnknownOrConflictingState(t *testing.T) {
	a, b := receiverTestKey(8), receiverTestKey(16)
	for _, test := range []struct {
		name     string
		reserved map[string]int
		pool     []int
	}{
		{"unknown reservations", nil, []int{51821}},
		{"unknown pool", map[string]int{}, nil},
		{"duplicate pool", map[string]int{}, []int{51821, 51821}},
		{"zero port", map[string]int{}, []int{0}},
		{"large port", map[string]int{}, []int{65536}},
		{"revoked retained port", map[string]int{a: 51821}, []int{51822}},
		{"duplicate reservation", map[string]int{a: 51821, b: 51821}, []int{51821, 51822}},
		{"invalid identity", map[string]int{"invalid": 51821}, []int{51821}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assigned, reserved, err := AssignReceivePorts([]ReceivePath{{PublicKey: a}}, test.reserved, test.pool)
			if err == nil || assigned != nil || reserved != nil {
				t.Fatal("unsafe reservation state returned an allocation")
			}
		})
	}
	for _, ports := range []map[string]int{nil, {}, {a: 0}, {a: 51821, b: 51822}} {
		if err := ValidateReceivePorts([]ReceivePath{{PublicKey: a}}, ports); err == nil {
			t.Fatal("inexact or invalid request ports accepted")
		}
	}
}
