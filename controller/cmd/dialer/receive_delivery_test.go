package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// Blocked callbacks exercise backpressure and joining, not native cleanup or
// receipt authentication. The TLS test below uses synthetic credentials and
// does not qualify controller completion, freshness acceptance or connectivity.
func TestReceiveRetirementDeliveriesBoundAndJoin(t *testing.T) {
	var active, peak atomic.Int32
	var d receiveRetirementDeliveries
	entered := make(chan int, 3)
	release := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	var once [3]sync.Once
	unblock := func(i int) { once[i].Do(func() { close(release[i]) }) }
	defer func() {
		for i := range release {
			unblock(i)
		}
	}()
	failures := []error{errors.New("first submission failed"), errors.New("second submission failed"), errors.New("third submission failed")}
	deliver := func(i int) func() error {
		return func() error {
			n := active.Add(1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			entered <- i
			<-release[i]
			active.Add(-1)
			return failures[i]
		}
	}
	d.submit(deliver(0))
	d.submit(deliver(1))
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("deliveries did not overlap")
		}
	}
	done := make(chan error, 1)
	later := errors.New("later admission failed")
	go func() {
		d.submit(deliver(2))
		// Match the dispatch early-return path: preserve both delivery failures
		// and the unrelated error, and do not leave submissions running.
		done <- errors.Join(later, d.wait())
	}()
	select {
	case <-entered:
		t.Fatal("more than two submissions in flight")
	case <-time.After(50 * time.Millisecond):
	}
	unblock(0)
	select {
	case i := <-entered:
		if i != 2 {
			t.Fatal("wrong pending submission")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backpressure did not release")
	}
	unblock(2)
	select {
	case <-done:
		t.Fatal("returned with a submission still running")
	case <-time.After(50 * time.Millisecond):
	}
	unblock(1)
	select {
	case err := <-done:
		for _, want := range append(failures, later) {
			if !errors.Is(err, want) {
				t.Fatal("lost a reconciliation error", err)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("submissions did not join")
	}
	if active.Load() != 0 || peak.Load() != 2 {
		t.Fatal("incorrect concurrency bound", active.Load(), peak.Load())
	}
}

func TestReceiveRetirementDeliveriesTLSCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := time.Now().UTC().Add(-time.Second)
	requests := make(chan handover.ReceiptRequest, 2)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request handover.ReceiptRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests <- request
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	defer close(release)
	client, err := handover.NewReceiptClient(server.URL, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var d receiveRetirementDeliveries
	for _, uid := range []string{"first", "second"} {
		d.submit(func() error {
			return client.Submit(ctx, "synthetic-token", handover.ReceiptRequest{MachineUID: uid, Receipts: []handover.PodReceipt{{Receipt: handover.Receipt{ObservedAt: observed}}}})
		})
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case r := <-requests:
			seen[r.MachineUID] = true
			if len(r.Receipts) != 1 || !r.Receipts[0].Receipt.ObservedAt.Equal(observed) {
				t.Fatal("delivery refreshed observation")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("TLS requests did not overlap")
		}
	}
	if !seen["first"] || !seen["second"] {
		t.Fatal("submission identity changed")
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- d.wait() }()
	select {
	case err := <-done:
		if err == nil || len(d.failures) != 2 {
			t.Fatal("cancelled deliveries were accepted or lost", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled deliveries escaped reconciliation")
	}
}
