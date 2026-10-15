package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
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
