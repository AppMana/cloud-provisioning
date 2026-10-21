package handover

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Exercise the real TLS client and handler across the former 15-second limit.
// Synthetic callbacks establish transport budgeting only, not native absence,
// authenticated retirement, observation freshness or live cleanup latency.
func TestReceiptSlowCompletionWithinSharedBudget(t *testing.T) {
	digest := strings.Repeat("c", 64)
	var deadline time.Time
	completed := false
	checkDeadline := func(ctx context.Context) {
		t.Helper()
		got, ok := ctx.Deadline()
		if !ok || !got.Equal(deadline) {
			t.Error("receipt stages did not share the original deadline")
		}
	}
	handler, err := NewReceiptCollectionHandler(func(ctx context.Context, _ string) (ReceiptCollection, error) {
		deadline, _ = ctx.Deadline()
		return ReceiptCollection{UID: "journal", Digest: digest, ExpiresAt: time.Now().Add(time.Minute), MaxAge: time.Minute,
			Members: []ReceiptMember{{NodeUID: "node"}},
			Observe: func(ctx context.Context, _ []PodReceipt) error { checkDeadline(ctx); return nil },
			Complete: func(ctx context.Context, _ []PodReceipt) error {
				checkDeadline(ctx)
				timer := time.NewTimer(16 * time.Second)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
					completed = true
					return nil
				}
			},
		}, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := NewReceiptClient(server.URL, ca)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.client.Timeout <= ReceiptRequestTimeout {
		t.Fatal("client transport expires before bounded handler work can complete")
	}
	request := ReceiptRequest{MachineUID: "machine", RoundHash: digest, Receipts: []PodReceipt{{Receipt: Receipt{NodeUID: "node", RoundHash: digest, ObservedAt: time.Now()}}}}
	if err := client.Submit(context.Background(), "private-token", request); err != nil {
		t.Fatal(err)
	}
	// Server.Close joins active handler work before inspecting shared state.
	server.Close()
	if !completed {
		t.Fatal("slow completion never committed")
	}
}

func TestReceiptLongerBudgetPreservesParentDeadline(t *testing.T) {
	digest := strings.Repeat("c", 64)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	parentDeadline, _ := ctx.Deadline()
	observed := false
	handler, err := NewReceiptCollectionHandler(func(ctx context.Context, _ string) (ReceiptCollection, error) {
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Equal(parentDeadline) {
			t.Error("shorter caller deadline was extended")
		}
		<-ctx.Done()
		return ReceiptCollection{UID: "journal", Digest: digest, ExpiresAt: time.Now().Add(time.Minute), MaxAge: time.Minute,
			Members:  []ReceiptMember{{NodeUID: "node"}},
			Observe:  func(context.Context, []PodReceipt) error { observed = true; return nil },
			Complete: func(context.Context, []PodReceipt) error { t.Error("cancelled source completed"); return nil },
		}, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(ReceiptRequest{MachineUID: "machine", RoundHash: digest, Receipts: []PodReceipt{{Receipt: Receipt{NodeUID: "node", RoundHash: digest, ObservedAt: time.Now()}}}})
	request := httptest.NewRequest(http.MethodPost, "https://controller/receipts", bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer private-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if observed || response.Code != http.StatusConflict || len(handler.batches) != 0 || len(handler.admissionSlots) != 0 {
		t.Fatalf("cancelled request retained work: observed=%v status=%d", observed, response.Code)
	}
}
