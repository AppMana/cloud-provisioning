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

// Diagnostic snapshots must remain bounded and contain no receipt envelope.
// Synthetic state does not establish live freshness or authentication.
func TestReceiptProgressBoundsAndAges(t *testing.T) {
	now := time.Now()
	target := ReceiptCollection{UID: "journal", Digest: "digest"}
	batch := receiptBatch{entries: map[[2]string]PodReceipt{}}
	for i := 0; i < 20; i++ {
		node := fmt.Sprintf("consumer-%d", i)
		target.Members = append(target.Members, ReceiptMember{NodeUID: node})
		batch.entries[[2]string{node, ""}] = PodReceipt{BearerToken: "must-not-escape", Receipt: Receipt{ObservedAt: now.Add(-3 * time.Second)}}
	}
	p := receiptProgress(target, batch, map[[2]string]bool{{"consumer-0", ""}: true}, now)
	if len(p.Consumers) != 16 || !p.Truncated || p.Required != 20 || p.Pending != 20 {
		t.Fatal("diagnostic bounds differ")
	}
	if !p.Consumers[0].Submitted || p.Consumers[1].Submitted || p.Consumers[0].AgeMillis != 3000 {
		t.Fatal("diagnostic age or submission differs")
	}
	if receiptProgressID("a", "bc") == receiptProgressID("ab", "c") {
		t.Fatal("identity components are ambiguous")
	}
	batch.entries[[2]string{"consumer-0", ""}] = PodReceipt{}
	if p.Consumers[0].AgeMillis != 3000 {
		t.Fatal("diagnostic retained mutable batch state")
	}
}

func TestReceiptProgressCompletionRejection(t *testing.T) {
	digest := strings.Repeat("e", 64)
	var report *ReceiptProgress
	h, err := NewReceiptCollectionHandler(func(context.Context, string) (ReceiptCollection, error) {
		return ReceiptCollection{UID: "journal", Digest: digest, ExpiresAt: time.Now().Add(time.Minute), MaxAge: time.Minute, Members: []ReceiptMember{{NodeUID: "node"}},
			Observe:  func(context.Context, []PodReceipt) error { return nil },
			Complete: func(context.Context, []PodReceipt) error { return fmt.Errorf("private-backend-error") },
		}, nil
	}, 1, func(p ReceiptProgress) { report = &p })
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(ReceiptRequest{MachineUID: "machine", RoundHash: digest, Receipts: []PodReceipt{{Receipt: Receipt{NodeUID: "node", RoundHash: digest, ObservedAt: time.Now()}}}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "https://controller/receipts", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer private-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusConflict || report == nil || report.Outcome != "completion-rejected" || len(h.batches) != 0 {
		t.Fatal("completion rejection or diagnostic changed")
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-token", "private-backend-error"} {
		if bytes.Contains(raw, []byte(private)) {
			t.Fatal("diagnostic exposed credential or backend error")
		}
	}
}
