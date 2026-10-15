package check

import (
	"context"
	"testing"
)

func TestReceiverSourceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, body, expected string
		pass                 bool
	}{
		{"ipv4", "172.29.0.31:4321", "172.29.0.31", true},
		{"ipv6", "[2001:db8::31]:4321", "2001:db8::31", true},
		{"mapped", "[::ffff:172.29.0.31]:4321", "172.29.0.31", true},
		{"snat", "172.29.0.29:4321", "172.29.0.31", false},
		{"ipv6 snat", "[2001:db8::29]:4321", "2001:db8::31", false},
		{"echo is not evidence", "ok", "172.29.0.31", false},
		{"missing port", "172.29.0.31", "172.29.0.31", false},
		{"invalid expected", "172.29.0.31:4321", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := sourceAddress(context.Background(), &fake{defaultBody: tc.body}, Pair{From: "a", To: "b"}, Source, "http://receiver/clientip", tc.expected)
			if r.OK != tc.pass {
				t.Fatalf("unexpected source verdict: %+v", r)
			}
		})
	}
}

func TestSourceChecksCoverBothDirectionsAndFamilies(t *testing.T) {
	targets := dualTargets()
	m := Run(context.Background(), &fake{defaultBody: "ok"}, targets, Options{Port: 8080, VerifySource: true, SkipTransfer: true})
	counts := kinds(m)
	if counts[Source] != 2 || counts[Source6] != 2 {
		t.Fatalf("missing source checks: %v", counts)
	}
	for _, r := range m.Results() {
		if (r.Kind == Source || r.Kind == Source6) && r.OK {
			t.Fatalf("reachability response accepted as source evidence: %+v", r)
		}
	}
}
