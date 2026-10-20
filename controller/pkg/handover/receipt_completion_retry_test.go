package handover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Synthetic callbacks cover collector retention, not native cleanup, token
// authentication or API persistence. Every subsequent completion must invoke
// the current backend with the original observations.
func TestReceiptCompletionFailurePreservesRetirementProgress(t *testing.T) {
	for _, scenario := range []string{"retry", "revoked", "binding", "incarnation", "age", "handover"} {
		t.Run(scenario, func(t *testing.T) {
			digest, uid := strings.Repeat("a", 64), "journal"
			age := 10 * time.Second
			observed := time.Now().Add(-time.Second)
			calls, resolutions := 0, 0
			h, err := NewReceiptCollectionHandler(func(context.Context, string) (ReceiptCollection, error) {
				resolutions++
				return ReceiptCollection{
					UID: uid, Digest: digest, ExpiresAt: time.Now().Add(time.Minute), MaxAge: age,
					ReadOnlyObserve: scenario != "handover",
					Members:         []ReceiptMember{{NodeUID: "first"}, {NodeUID: "second"}},
					Observe:         func(context.Context, []PodReceipt) error { return nil },
					Complete: func(_ context.Context, entries []PodReceipt) error {
						calls++
						if len(entries) != 2 {
							t.Fatal("completion lost a consumer")
						}
						for _, entry := range entries {
							if entry.Receipt.NodeUID == "first" && (!entry.Receipt.ObservedAt.Equal(observed) || entry.BearerToken != "first-token") {
								t.Fatal("retained evidence was renewed or rebound")
							}
						}
						if calls == 1 || scenario == "revoked" {
							return fmt.Errorf("completion rejected")
						}
						return nil
					},
				}, nil
			}, 1)
			if err != nil {
				t.Fatal(err)
			}
			submit := func(node string, at time.Time) int {
				body, _ := json.Marshal(ReceiptRequest{MachineUID: "machine", RoundHash: digest, Receipts: []PodReceipt{{Receipt: Receipt{NodeUID: node, RoundHash: digest, ObservedAt: at}}}})
				r := httptest.NewRequest(http.MethodPost, "https://controller/receipts", bytes.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+node+"-token")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w.Code
			}
			if submit("first", observed) != http.StatusAccepted || submit("second", time.Now()) != http.StatusConflict || calls != 1 {
				t.Fatal("fixture did not reach a failed completion")
			}
			if scenario == "handover" {
				if len(h.batches) != 0 {
					t.Fatal("mutating handover retained its rejected batch")
				}
			} else if len(h.batches["machine"].entries) != 2 {
				t.Fatal("failed retirement commit discarded independently admitted consumers")
			}
			switch scenario {
			case "binding":
				digest = strings.Repeat("b", 64)
			case "incarnation":
				uid = "replacement"
			case "age":
				age = 500 * time.Millisecond
			}
			status := submit("second", time.Now())
			switch scenario {
			case "retry":
				if status != http.StatusNoContent || calls != 2 || len(h.batches) != 0 {
					t.Fatal("fresh submission did not reauthenticate and complete retained evidence")
				}
			case "revoked":
				if status != http.StatusConflict || calls != 2 {
					t.Fatal("retained evidence bypassed current completion authentication")
				}
			default:
				if status != http.StatusAccepted || calls != 1 || len(h.batches["machine"].entries) != 1 {
					t.Fatal("changed authority or age reused retained evidence")
				}
			}
			if resolutions != 3 {
				t.Fatal("retry skipped fresh source resolution")
			}
		})
	}
}
