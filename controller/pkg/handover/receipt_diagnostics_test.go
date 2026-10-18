package handover

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Synthetic callback failures exercise diagnostic boundaries only. These tests
// neither authenticate real credentials nor establish native retirement.
func TestReceiptFailureStagesDoNotExposeCallbackErrors(t *testing.T) {
	for _, stage := range []string{"source", "observation", "completion", "source-deadline"} {
		t.Run(stage, func(t *testing.T) {
			digest := strings.Repeat("a", 64)
			private := errors.New("private-token and arbitrary backend details")
			h, err := NewReceiptCollectionHandler(func(context.Context, string) (ReceiptCollection, error) {
				if strings.HasPrefix(stage, "source") {
					return ReceiptCollection{}, private
				}
				return ReceiptCollection{
					UID: "journal", Digest: digest, ExpiresAt: time.Now().Add(time.Minute), MaxAge: time.Minute,
					Members: []ReceiptMember{{NodeUID: "node"}},
					Observe: func(context.Context, []PodReceipt) error {
						if stage == "observation" {
							return private
						}
						return nil
					},
					Complete: func(context.Context, []PodReceipt) error { return private },
				}, nil
			}, 1)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(ReceiptRequest{MachineUID: "machine", RoundHash: digest, Receipts: []PodReceipt{{BootID: "boot", Receipt: Receipt{NodeUID: "node", AgentContainerID: "container", RoundHash: digest, ObservedAt: time.Now()}}}})
			r := httptest.NewRequest(http.MethodPost, "https://controller/receipts", bytes.NewReader(body))
			r.Header.Set("Authorization", "Bearer private-token")
			if strings.HasSuffix(stage, "-deadline") {
				ctx, cancel := context.WithDeadline(r.Context(), time.Now().Add(-time.Second))
				defer cancel()
				r = r.WithContext(ctx)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusConflict || w.Header().Get(receiptFailureStageHeader) != stage {
				t.Fatalf("incorrect rejection stage: %d %v", w.Code, w.Header())
			}
			if strings.Contains(w.Body.String(), "private") || strings.Contains(w.Header().Get(receiptFailureStageHeader), "private") {
				t.Fatal("rejection exposed callback error or credential")
			}
		})
	}
}

func TestReceiptFailureStageAllowlist(t *testing.T) {
	for _, raw := range []string{"source", "observation", "completion-deadline"} {
		if receiptFailureStage(raw) != raw {
			t.Fatal("known stage lost")
		}
	}
	for _, raw := range []string{"", "private-token", "source private-token", "source-deadline-private-token", "source-deadline-deadline"} {
		if receiptFailureStage(raw) != "unknown" {
			t.Fatal("arbitrary peer header reached diagnostics")
		}
	}
}
