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

// A rejected source must not queue behind an unrelated collection's commit or
// consume an admission slot while waiting for that commit to finish.
func TestReceiptSourceRejectionDoesNotWaitForCollector(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "canceled"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h, err := NewReceiptCollectionHandler(func(context.Context, string) (ReceiptCollection, error) {
				if canceled {
					cancel()
					return ReceiptCollection{}, nil
				}
				return ReceiptCollection{}, errors.New("private source error")
			}, 1)
			if err != nil {
				t.Fatal(err)
			}
			h.gate <- struct{}{}
			digest := strings.Repeat("a", 64)
			body, _ := json.Marshal(ReceiptRequest{MachineUID: "machine", RoundHash: digest, Receipts: []PodReceipt{{Receipt: Receipt{RoundHash: digest}}}})
			r := httptest.NewRequest(http.MethodPost, "https://controller/receipts", bytes.NewReader(body)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer private-token")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				h.ServeHTTP(w, r)
				close(done)
			}()
			defer func() {
				cancel()
				<-h.gate
				<-done
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("rejected source waited for the collector")
			}
			if w.Code != http.StatusConflict || w.Header().Get(receiptFailureStageHeader) != "source" || w.Header().Get("Retry-After") != "" {
				t.Fatalf("source rejection misclassified: %d %v", w.Code, w.Header())
			}
			if len(h.admissionSlots) != 0 || len(h.batches) != 0 || len(h.gate) != 1 {
				t.Fatal("source rejection retained admission or changed collector state")
			}
			if strings.Contains(w.Body.String(), "private") {
				t.Fatal("source rejection exposed credentials or backend errors")
			}
		})
	}
}
