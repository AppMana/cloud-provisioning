package handover

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReceiptClientTLSAndRedirects(t *testing.T) {
	called := 0
	status := http.StatusAccepted
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Header.Get("Authorization") != "Bearer private-token" {
			t.Error("missing credential")
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "private-token") {
			t.Error("credential serialized in body")
		}
		w.Header().Set("Location", "/must-not-follow")
		w.WriteHeader(status)
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := NewReceiptClient(server.URL, ca)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := ReceiptRequest{Receipts: []PodReceipt{{BearerToken: "private-token"}}}
	if err := client.Submit(context.Background(), "private-token", request); err != nil {
		t.Fatal(err)
	}
	status = http.StatusTemporaryRedirect
	if err := client.Submit(context.Background(), "private-token", request); err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("redirect accepted or credential leaked")
	}
	if called != 2 {
		t.Fatal("redirect followed")
	}
	for _, endpoint := range []string{"http://controller/receipts", "https://user:password@controller/receipts", "https://controller/receipts?token=secret", "https://controller/receipts#fragment"} {
		if _, err := NewReceiptClient(endpoint, ca); err == nil {
			t.Fatal("unsafe endpoint accepted")
		}
	}
	if _, err := NewReceiptClient(server.URL, nil); err == nil {
		t.Fatal("missing CA accepted")
	}
	// The default client has no trust in this self-signed endpoint.
	untrusted := &ReceiptClient{endpoint: server.URL, client: &http.Client{Timeout: time.Second}}
	if err := untrusted.Submit(context.Background(), "private-token", request); err == nil {
		t.Fatal("untrusted TLS accepted")
	}
	if called != 2 {
		t.Fatal("untrusted endpoint received a credential")
	}
}

// Transport-only contention coverage. Synthetic callbacks do not establish
// native absence or authenticated completion; production admission still owns
// identity, source and freshness checks on every delivery.
func TestReceiptClientRetriesOnlyExplicitBusy(t *testing.T) {
	for _, scenario := range []string{"busy", "conflict", "unavailable", "cancel", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			var first []byte
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 100*time.Millisecond)
				defer stop()
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				raw, _ := io.ReadAll(r.Body)
				if r.Header.Get("Authorization") != "Bearer private-token" {
					t.Error("credential missing on delivery")
				}
				if calls == 1 {
					first = raw
				} else if !bytes.Equal(first, raw) {
					t.Error("retry changed observation or freshness")
				}
				if calls > 1 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				if scenario == "busy" || scenario == "cancel" || scenario == "deadline" {
					w.Header().Set("Retry-After", "1")
				}
				if scenario == "cancel" {
					cancel()
				}
				if scenario == "conflict" {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(http.StatusConflict)
				} else {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			client, err := NewReceiptClient(server.URL, ca)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			request := ReceiptRequest{MachineUID: "machine", Receipts: []PodReceipt{{Receipt: Receipt{ObservedAt: time.Now()}}}}
			err = client.Submit(ctx, "private-token", request)
			if scenario == "busy" {
				if err != nil || calls != 2 {
					t.Fatalf("busy observation not redelivered: calls=%d err=%v", calls, err)
				}
			} else if err == nil || calls != 1 || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("unsafe retry or error: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestReceiptCollectorSignalsBusyBeforeAdmission(t *testing.T) {
	called := false
	h, err := NewReceiptCollectionHandler(func(context.Context, string) (ReceiptCollection, error) {
		called = true
		return ReceiptCollection{}, fmt.Errorf("must not resolve")
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	h.gate <- struct{}{}
	defer func() { <-h.gate }()
	digest := strings.Repeat("a", 64)
	body, _ := json.Marshal(ReceiptRequest{MachineUID: "machine", RoundHash: digest, Receipts: []PodReceipt{{Receipt: Receipt{RoundHash: digest}}}})
	r := httptest.NewRequest(http.MethodPost, "https://controller/receipts", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer private-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" || called {
		t.Fatal("busy response did not precede admission")
	}
}

func TestReceiptConcurrentDelivery(t *testing.T) {
	digest := strings.Repeat("b", 64)
	observed := time.Now()
	var members []ReceiptMember
	for i := 0; i < 7; i++ {
		members = append(members, ReceiptMember{NodeUID: fmt.Sprintf("node-%d", i)})
	}
	completed := make(chan struct{}, 1)
	h, err := NewReceiptCollectionHandler(func(ctx context.Context, _ string) (ReceiptCollection, error) {
		// Hold the real collector gate while modeling an admission API read.
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return ReceiptCollection{}, ctx.Err()
		}
		return ReceiptCollection{
			UID: "journal", Digest: digest, ExpiresAt: observed.Add(10 * time.Second), MaxAge: 10 * time.Second, Members: members,
			Observe: func(_ context.Context, entries []PodReceipt) error {
				for _, e := range entries {
					if e.BearerToken != "private-token" || !e.Receipt.ObservedAt.Equal(observed) || time.Since(e.Receipt.ObservedAt) >= 10*time.Second {
						return fmt.Errorf("credential or observation changed or expired")
					}
				}
				return nil
			},
			Complete: func(_ context.Context, entries []PodReceipt) error {
				if len(entries) != len(members) {
					return fmt.Errorf("incomplete consumer batch")
				}
				completed <- struct{}{}
				return nil
			},
		}, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(h)
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := NewReceiptClient(server.URL, ca)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results := make(chan error, len(members))
	for _, member := range members {
		go func() {
			results <- client.Submit(ctx, "private-token", ReceiptRequest{MachineUID: "machine", RoundHash: digest, Receipts: []PodReceipt{{BootID: "boot", Receipt: Receipt{NodeUID: member.NodeUID, AgentContainerID: "container", RoundHash: digest, ObservedAt: observed}}}})
		}()
	}
	for range members {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if len(completed) != 1 {
		t.Fatal("concurrent submissions did not complete one batch")
	}
}

func TestReceiptCollectorExpiryAndShutdown(t *testing.T) {
	h, _ := NewReceiptHandler(func(context.Context, string) (Store, map[string]PodReceiptAuthenticator, error) {
		return Store{}, nil, fmt.Errorf("unused")
	}, 1)
	h.batches["expired"] = receiptBatch{expires: time.Now().Add(-time.Second), entries: map[[2]string]PodReceipt{{"node", "device"}: {BearerToken: "private-token"}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		h.gate <- struct{}{}
		empty := len(h.batches) == 0
		<-h.gate
		if empty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired credentials retained")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.gate <- struct{}{}
	h.batches["pending"] = receiptBatch{expires: time.Now().Add(time.Hour)}
	<-h.gate
	cancel()
	<-done
	if len(h.batches) != 0 || !h.stopped {
		t.Fatal("shutdown retained pending credentials")
	}
}

func TestReceiptTransportRejectsUnprotectedRequests(t *testing.T) {
	called := false
	h, err := NewReceiptHandler(func(context.Context, string) (Store, map[string]PodReceiptAuthenticator, error) {
		called = true
		return Store{}, nil, fmt.Errorf("must not resolve")
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "http://controller/receipts", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer private-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUpgradeRequired || called || strings.Contains(w.Body.String(), "private-token") {
		t.Fatal("unprotected submission reached admission")
	}
	server := httptest.NewTLSServer(h)
	defer server.Close()
	for _, body := range []string{`{}`, `{"unexpected":"private-token"}`, strings.Repeat("x", (1<<20)+1)} {
		r, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer private-token")
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest || called {
			t.Fatal("invalid submission reached admission")
		}
	}
}

// Run with the existing real API fixture: actual pod-bound tokens, TLS and
// journal resourceVersion checks exercise the complete transport boundary.
func verifyReceiptTransport(t *testing.T, verifier PodReceiptAuthenticator, nodes []*corev1.Node, bearers []string) {
	t.Helper()
	ctx := context.Background()
	for _, scenario := range []string{"advance", "restart", "revoked-container"} {
		t.Run("transport-"+scenario, func(t *testing.T) {
			spec := fixture(t).Spec
			policies := map[string]PodReceiptAuthenticator{}
			for i := range spec.Members {
				spec.Members[i].NodeUID = string(nodes[i].UID)
				policies[string(nodes[i].UID)] = verifier
			}
			transition, err := NewTransition(spec, time.Now(), Policy{RoundLifetime: time.Minute, MaxReceiptAge: 30 * time.Second, DrainInterval: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			store := Store{ConfigMaps: verifier.API.CoreV1().ConfigMaps(verifier.Namespace), Name: "transport-" + scenario, ClusterUID: spec.ClusterUID}
			snapshot, err := store.Create(ctx, transition)
			if err != nil {
				t.Fatal(err)
			}
			wrongIncarnation := store
			wrongIncarnation.ExpectedUID = "previous-journal-uid"
			if _, err := wrongIncarnation.Load(ctx); err == nil {
				t.Fatal("recreated journal passed incarnation admission")
			}
			view, _ := snapshot.Transition().View()
			digest, _ := view.Round.Digest()
			resolve := func(_ context.Context, machine string) (Store, map[string]PodReceiptAuthenticator, error) {
				if machine != "machine" {
					return Store{}, nil, fmt.Errorf("unknown machine")
				}
				return store, policies, nil
			}
			handler, _ := NewReceiptHandler(resolve, 1)
			server := httptest.NewTLSServer(handler)
			defer server.Close()
			submit := func(index int) int {
				t.Helper()
				m := view.Round.Members[index]
				for _, member := range view.Round.Members {
					if member.NodeUID == string(nodes[index].UID) {
						m = member
						break
					}
				}
				request := ReceiptRequest{MachineUID: "machine", RoundHash: digest, Receipts: []PodReceipt{{BootID: nodes[index].Status.NodeInfo.BootID, Receipt: Receipt{AgentContainerID: "containerd://running-agent", RoundHash: digest, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: time.Now()}}}}
				body, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				r, _ := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+bearers[index])
				response, err := server.Client().Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				raw, _ := io.ReadAll(response.Body)
				for _, bearer := range bearers {
					if strings.Contains(string(raw), bearer) {
						t.Fatal("response leaked credential")
					}
				}
				return response.StatusCode
			}
			if status := submit(0); status != http.StatusAccepted {
				t.Fatalf("partial status %d", status)
			}
			expected := http.StatusNoContent
			switch scenario {
			case "restart":
				server.Close()
				handler, _ = NewReceiptHandler(resolve, 1)
				server = httptest.NewTLSServer(handler)
				defer server.Close()
				expected = http.StatusAccepted
			case "revoked-container":
				pod, err := verifier.API.CoreV1().Pods(verifier.Namespace).Get(ctx, nodes[0].Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				original := pod.Status.DeepCopy()
				pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
				if _, err := verifier.API.CoreV1().Pods(verifier.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					current, err := verifier.API.CoreV1().Pods(verifier.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
					if err != nil {
						t.Error(err)
						return
					}
					current.Status = *original
					if _, err := verifier.API.CoreV1().Pods(verifier.Namespace).UpdateStatus(ctx, current, metav1.UpdateOptions{}); err != nil {
						t.Error(err)
					}
				})
				expected = http.StatusConflict
			}
			if status := submit(1); status != expected {
				t.Fatalf("completion status %d, want %d", status, expected)
			}
			if scenario == "restart" {
				if status := submit(0); status != http.StatusNoContent {
					t.Fatalf("resubmission status %d", status)
				}
			}
			current, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			v, _ := current.Transition().View()
			want := Switch
			if scenario == "revoked-container" {
				want = Prepare
			}
			if v.Round.Phase != want {
				t.Fatalf("phase %s, want %s", v.Round.Phase, want)
			}
			object, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(object)
			for _, bearer := range bearers {
				if strings.Contains(string(raw), bearer) {
					t.Fatal("journal leaked credential")
				}
			}
			if len(handler.batches) != 0 {
				t.Fatal("completed or rejected batch retained credentials")
			}
		})
	}
}

func TestReceiptBatchExpiryPreservesFreshParticipants(t *testing.T) {
	now := time.Now()
	stale, fresh := [2]string{"old", "device"}, [2]string{"fresh", "device"}
	batch := receiptBatch{expires: now.Add(time.Minute), maxAge: 30 * time.Second, entries: map[[2]string]PodReceipt{
		stale: {BearerToken: "expired-token", Receipt: Receipt{ObservedAt: now.Add(-30 * time.Second)}},
		fresh: {BearerToken: "fresh-token", Receipt: Receipt{ObservedAt: now.Add(-5 * time.Second)}},
	}}
	if !batch.prune(now) || len(batch.entries) != 1 || batch.entries[fresh].BearerToken != "fresh-token" {
		t.Fatal("expiry discarded a fresh participant")
	}
	if _, found := batch.entries[stale]; found {
		t.Fatal("expired credential retained")
	}
	// A new observation can join the retained participant without replaying
	// credentials from the expired observation.
	batch.entries[stale] = PodReceipt{BearerToken: "replacement-token", Receipt: Receipt{ObservedAt: now}}
	if !batch.prune(now.Add(20*time.Second)) || len(batch.entries) != 2 {
		t.Fatal("fresh observations cannot accumulate")
	}
	if batch.prune(now.Add(30*time.Second)) || len(batch.entries) != 0 {
		t.Fatal("observation expiry retained credentials")
	}
	batch.entries[fresh] = PodReceipt{BearerToken: "fresh-token", Receipt: Receipt{ObservedAt: now.Add(59 * time.Second)}}
	if batch.prune(batch.expires) || len(batch.entries) != 0 {
		t.Fatal("round expiry retained credentials")
	}
}
