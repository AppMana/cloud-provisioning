package attachment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// Real TokenReview and TLS exercise concurrent authentication against one
// reserved inventory revision. Synthetic native proofs do not qualify cleanup,
// multi-node capacity, workload continuity or reservation release.
func verifyConcurrentRetirementAuthentication(t *testing.T, api kubernetes.Interface, store handover.Store, mesh *corev1.Secret, machineUID, inventoryUID string, policy handover.PodReceiptAuthenticator, submission handover.PodReceipt) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	snapshot, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewReceiveRetirementReceiptScope(ctx, store.ConfigMaps, store.ClusterUID, inventoryUID, snapshot, mesh)
	if err != nil {
		t.Fatal(err)
	}
	authenticated := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	handler, err := handover.NewReceiptCollectionHandler(func(requestCtx context.Context, machine string) (handover.ReceiptCollection, error) {
		if machine != machineUID {
			return handover.ReceiptCollection{}, fmt.Errorf("unknown fixture machine")
		}
		source, err := ReadReceiveRetirementSet(requestCtx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machine)
		if err != nil {
			return handover.ReceiptCollection{}, err
		}
		collection, err := source.ReceiptCollection(requestCtx, api, store, map[string]handover.PodReceiptAuthenticator{submission.Receipt.NodeUID: policy}, time.Minute, func() error { return nil })
		if err != nil {
			return handover.ReceiptCollection{}, err
		}
		if !collection.ReadOnlyObserve {
			return handover.ReceiptCollection{}, fmt.Errorf("retirement authentication unexpectedly serial")
		}
		observe := collection.Observe
		collection.Observe = func(ctx context.Context, entries []handover.PodReceipt) error {
			if err := observe(ctx, entries); err != nil {
				return err
			}
			authenticated <- struct{}{}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return collection, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	defer func() { cancel(); unblock(); server.Close() }()
	public := submission
	public.BearerToken = ""
	public.Receipt.RoundHash = scope.Digest()
	public.Receipt.ObservedAt = time.Now()
	body, err := json.Marshal(handover.ReceiptRequest{MachineUID: machineUID, RoundHash: scope.Digest(), Receipts: []handover.PodReceipt{public}})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan int, 2)
	for range 2 {
		go func() {
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
			if err != nil {
				results <- 0
				return
			}
			request.Header.Set("Authorization", "Bearer "+submission.BearerToken)
			response, err := server.Client().Do(request)
			if err != nil {
				results <- 0
				return
			}
			response.Body.Close()
			results <- response.StatusCode
		}()
	}
	for range 2 {
		select {
		case <-authenticated:
		case <-time.After(5 * time.Second):
			t.Fatal("real-token observations did not authenticate concurrently")
		}
	}
	unblock()
	statuses := map[int]int{}
	for range 2 {
		select {
		case code := <-results:
			statuses[code]++
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent authenticated requests did not finish")
		}
	}
	if statuses[http.StatusNoContent] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("expected one commit and one stale-source rejection: %v", statuses)
	}
	verified, err := VerifyReceiveRetirementCompletion(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, inventoryUID, time.Minute)
	if err != nil || !verified {
		t.Fatal("concurrent completion did not preserve verifiable authenticated evidence", err)
	}
}
