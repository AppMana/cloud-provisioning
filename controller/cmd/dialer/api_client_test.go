package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

func TestAPIClientBoundsStalledResponse(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	config := &rest.Config{Host: server.URL}
	client, err := newAPIClient(config)
	if err != nil {
		t.Fatal(err)
	}
	if config.Timeout != 0 {
		t.Fatal("mutated caller's client configuration")
	}
	// A longer caller deadline proves that the client's own deadline releases
	// a reconciliation blocked on a response body, not just on connection setup.
	ctx, cancel := context.WithTimeout(context.Background(), 2*apiRequestTimeout)
	defer cancel()
	_, err = client.CoreV1().Secrets("test").Get(ctx, "peers", metav1.GetOptions{})
	if err == nil {
		t.Fatal("accepted an incomplete Secret response")
	}
	select {
	case <-started:
	default:
		t.Fatal("request never reached server")
	}
	if ctx.Err() != nil {
		t.Fatal("API client waited for the caller deadline instead of its own")
	}
}

func TestAPIClientPreservesShorterDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := newAPIClient(&rest.Config{Host: server.URL, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = client.CoreV1().Secrets("test").Get(ctx, "peers", metav1.GetOptions{})
	if err == nil || ctx.Err() != nil {
		t.Fatalf("shorter deadline was not retained: %v", err)
	}
}

// Repeated admissions must reach a responsive server without exhausting their
// deadline in a default local token bucket. This covers client throttling only;
// it does not prove native completion, server capacity or multi-node convergence.
func TestAPIClientAllowsRepeatedReceiptAdmission(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"peers","namespace":"test"}}`))
	}))
	defer server.Close()
	config := &rest.Config{Host: server.URL}
	client, err := newAPIClient(config)
	if err != nil {
		t.Fatal(err)
	}
	if config.QPS != 0 || config.Burst != 0 {
		t.Fatal("mutated caller's rate budget")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 0; i < 5*36; i++ {
		if _, err := client.CoreV1().Secrets("test").Get(ctx, "peers", metav1.GetOptions{}); err != nil {
			t.Fatalf("responsive API admission throttled at read %d: %v", i+1, err)
		}
	}
}

func TestAPIClientPreservesExplicitRateBudget(t *testing.T) {
	client, err := newAPIClient(&rest.Config{Host: "https://example.invalid", QPS: 7, Burst: 3})
	if err != nil {
		t.Fatal(err)
	}
	limiter := client.CoreV1().RESTClient().GetRateLimiter()
	if limiter.QPS() != 7 {
		t.Fatal("explicit request rate changed")
	}
	for i := 0; i < 3; i++ {
		if !limiter.TryAccept() {
			t.Fatal("explicit burst reduced")
		}
	}
	if limiter.TryAccept() {
		t.Fatal("explicit burst exceeded")
	}
}

func TestAPIClientPreservesExplicitLimiter(t *testing.T) {
	limiter := flowcontrol.NewTokenBucketRateLimiter(7, 3)
	config := &rest.Config{Host: "https://example.invalid", RateLimiter: limiter}
	client, err := newAPIClient(config)
	if err != nil {
		t.Fatal(err)
	}
	if client.CoreV1().RESTClient().GetRateLimiter() != limiter || config.QPS != 0 || config.Burst != 0 {
		t.Fatal("explicit limiter replaced or caller configuration changed")
	}
}
