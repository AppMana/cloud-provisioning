package attachment

import (
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// These are policy-admission tests using validated synthetic journals. They do
// not qualify native cleanup timing or provider convergence.
func TestReceiveRetirementReceiptAge(t *testing.T) {
	_, snapshot, _, _ := retirementFixture(t, "forward", true)
	for _, tc := range []struct {
		name    string
		ceiling time.Duration
		ok      bool
	}{
		{"same-policy", 10 * time.Second, true},
		{"increased-policy-keeps-old-bound", 2 * time.Minute, true},
		{"decreased-policy-rejects", 5 * time.Second, false},
		{"zero-policy-rejects", 0, false},
		{"negative-policy-rejects", -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			age, err := ReceiveRetirementReceiptAge(snapshot, tc.ceiling)
			if (err == nil) != tc.ok {
				t.Fatalf("age=%s err=%v", age, err)
			}
			if tc.ok && age != 10*time.Second {
				t.Fatalf("existing journal freshness changed: %s", age)
			}
			if !tc.ok && age != 0 {
				t.Fatal("rejected policy returned usable freshness")
			}
		})
	}
	if _, err := ReceiveRetirementReceiptAge(handover.Snapshot{}, time.Minute); err == nil {
		t.Fatal("unvalidated journal admitted")
	}
}
