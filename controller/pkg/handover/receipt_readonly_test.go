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

// Synthetic callbacks exercise collector scheduling only. These tests do not
// prove token authentication, native absence, live-source fencing or API CAS.
func readonlyFixture(machine string) ReceiptCollection {
	return ReceiptCollection{UID: machine, Digest: strings.Repeat("a", 64), ExpiresAt: time.Now().Add(time.Minute), MaxAge: 30 * time.Second, Members: []ReceiptMember{{NodeUID: "first"}, {NodeUID: "second"}}, ReadOnlyObserve: true,
		Observe: func(context.Context, []PodReceipt) error { return nil }, Complete: func(context.Context, []PodReceipt) error { return fmt.Errorf("unexpected completion") }}
}

func readonlyRequest(h *ReceiptHandler, ctx context.Context, machine, digest string, at time.Time) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	raw, _ := json.Marshal(ReceiptRequest{MachineUID: machine, RoundHash: digest, Receipts: []PodReceipt{{Receipt: Receipt{NodeUID: "first", RoundHash: digest, ObservedAt: at}}}})
	r := httptest.NewRequest(http.MethodPost, "https://controller/receipts", bytes.NewReader(raw)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer synthetic-token")
	go func() { w := httptest.NewRecorder(); h.ServeHTTP(w, r); done <- w }()
	return done
}

func readonlyResult(t *testing.T, done <-chan *httptest.ResponseRecorder, status int) {
	t.Helper()
	select {
	case w := <-done:
		if w.Code != status {
			t.Fatalf("status=%d stage=%s want=%d", w.Code, w.Header().Get(receiptFailureStageHeader), status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not finish")
	}
}

func TestReceiptReadOnlyObservationConcurrency(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(readOnly), func(t *testing.T) {
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			defer close(release)
			h, err := NewReceiptCollectionHandler(func(_ context.Context, machine string) (ReceiptCollection, error) {
				c := readonlyFixture(machine)
				c.ReadOnlyObserve = readOnly
				c.Observe = func(ctx context.Context, _ []PodReceipt) error {
					entered <- struct{}{}
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return c, nil
			}, 2)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := readonlyRequest(h, ctx, "one", strings.Repeat("a", 64), time.Now())
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first observation did not start")
			}
			second := readonlyRequest(h, ctx, "two", strings.Repeat("a", 64), time.Now())
			if readOnly {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("read-only observations serialized")
				}
			} else {
				select {
				case <-entered:
					t.Fatal("mutating observations overlapped")
				case <-time.After(50 * time.Millisecond):
				}
			}
			// Cancellation releases callbacks and the queue without leaking requests.
			cancel()
			for _, done := range []<-chan *httptest.ResponseRecorder{first, second} {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("cancelled request remained live")
				}
			}
			if len(h.gate) != 0 || len(h.admissionSlots) != 0 {
				t.Fatal("collector slots leaked")
			}
		})
	}
}

func TestReceiptReadOnlyCompletionRemainsSerial(t *testing.T) {
	observed := make(chan struct{}, 2)
	authenticate := make(chan struct{})
	defer close(authenticate)
	commits := make(chan struct{}, 2)
	finish := make(chan struct{})
	defer close(finish)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := NewReceiptCollectionHandler(func(_ context.Context, machine string) (ReceiptCollection, error) {
		c := readonlyFixture(machine)
		c.Members = c.Members[:1]
		c.Observe = func(ctx context.Context, _ []PodReceipt) error {
			observed <- struct{}{}
			select {
			case <-authenticate:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		c.Complete = func(ctx context.Context, _ []PodReceipt) error {
			commits <- struct{}{}
			select {
			case <-finish:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return c, nil
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	first := readonlyRequest(h, ctx, "one", strings.Repeat("a", 64), time.Now())
	second := readonlyRequest(h, ctx, "two", strings.Repeat("a", 64), time.Now())
	for range 2 {
		select {
		case <-observed:
		case <-time.After(5 * time.Second):
			t.Fatal("authentication did not overlap")
		}
	}
	authenticate <- struct{}{}
	authenticate <- struct{}{}
	select {
	case <-commits:
	case <-time.After(5 * time.Second):
		t.Fatal("completion did not start")
	}
	select {
	case <-commits:
		t.Fatal("completion writes overlapped")
	case <-time.After(50 * time.Millisecond):
	}
	finish <- struct{}{}
	select {
	case <-commits:
	case <-time.After(5 * time.Second):
		t.Fatal("second completion did not start")
	}
	finish <- struct{}{}
	readonlyResult(t, first, http.StatusNoContent)
	readonlyResult(t, second, http.StatusNoContent)
}

func TestReceiptReadOnlyRechecksAfterAuthentication(t *testing.T) {
	for _, fault := range []string{"expiry", "queued-expiry", "collection-expiry", "future", "shutdown", "capacity", "replacement", "cancel", "authentication"} {
		t.Run(fault, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := readonlyFixture("one")
			if fault == "queued-expiry" {
				c.MaxAge = 50 * time.Millisecond
			}
			if fault == "collection-expiry" {
				c.ExpiresAt = time.Now().Add(2 * time.Second)
			}
			at := time.Now()
			if fault == "expiry" {
				at = at.Add(-time.Minute)
			}
			if fault == "future" {
				at = at.Add(time.Minute)
			}
			c.Observe = func(context.Context, []PodReceipt) error {
				close(entered)
				<-release
				if fault == "authentication" {
					return fmt.Errorf("source changed during authentication")
				}
				return nil
			}
			h, err := NewReceiptCollectionHandler(func(context.Context, string) (ReceiptCollection, error) { return c, nil }, 1)
			if err != nil {
				t.Fatal(err)
			}
			done := readonlyRequest(h, ctx, "one", c.Digest, at)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("authentication did not start")
			}
			// The held gate models another collector operation while authentication
			// runs. It also verifies that read-only Observe does not own the gate.
			select {
			case h.gate <- struct{}{}:
			case <-time.After(time.Second):
				t.Fatal("authentication held collector gate")
			}
			sentinel := receiptBatch{uid: "newer", round: strings.Repeat("b", 64), expires: time.Now().Add(time.Minute), maxAge: time.Minute, entries: map[[2]string]PodReceipt{{"second", ""}: {Receipt: Receipt{ObservedAt: time.Now()}}}}
			expected := http.StatusConflict
			switch fault {
			case "shutdown":
				h.stopped = true
				expected = http.StatusServiceUnavailable
			case "capacity":
				h.batches["other"] = sentinel
				expected = http.StatusServiceUnavailable
			case "replacement":
				h.batches["one"] = sentinel
			case "authentication":
				sentinel.uid, sentinel.round = c.UID, c.Digest
				h.batches["one"] = sentinel
			case "cancel":
				cancel()
				expected = http.StatusServiceUnavailable
			}
			if fault == "queued-expiry" || fault == "collection-expiry" {
				release <- struct{}{}
				deadline := at.Add(c.MaxAge)
				if fault == "collection-expiry" {
					deadline = c.ExpiresAt
				}
				time.Sleep(time.Until(deadline) + 20*time.Millisecond)
				<-h.gate
			} else {
				<-h.gate
				release <- struct{}{}
			}
			readonlyResult(t, done, expected)
			if fault == "replacement" {
				if h.batches["one"].uid != "newer" {
					t.Fatal("stale admission replaced a newer batch")
				}
			} else if fault == "authentication" {
				if len(h.batches["one"].entries) != 1 {
					t.Fatal("rejected authentication erased independent proof")
				}
			} else if _, ok := h.batches["one"]; ok {
				t.Fatal("rejected observation retained")
			}
			if len(h.gate) != 0 || len(h.admissionSlots) != 0 {
				t.Fatal("collector slots leaked")
			}
		})
	}
}

func TestReceiptReadOnlyKeepsNewerObservation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	old := time.Now().Add(-time.Second)
	newer := time.Now()
	c := readonlyFixture("one")
	c.Observe = func(_ context.Context, entries []PodReceipt) error {
		if entries[0].Receipt.ObservedAt.Equal(old) {
			close(entered)
			<-release
		}
		return nil
	}
	h, err := NewReceiptCollectionHandler(func(context.Context, string) (ReceiptCollection, error) {
		resolved := c
		resolved.ExpiresAt = time.Now().Add(time.Minute)
		return resolved, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	first := readonlyRequest(h, context.Background(), "one", c.Digest, old)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old observation did not start")
	}
	readonlyResult(t, readonlyRequest(h, context.Background(), "one", c.Digest, newer), http.StatusAccepted)
	newerExpiry := h.batches["one"].expires
	release <- struct{}{}
	readonlyResult(t, first, http.StatusAccepted)
	if !h.batches["one"].entries[[2]string{"first", ""}].Receipt.ObservedAt.Equal(newer) {
		t.Fatal("older observation erased newer proof")
	}
	if !h.batches["one"].expires.Equal(newerExpiry) {
		t.Fatal("older collection shortened renewed observation window")
	}
}
