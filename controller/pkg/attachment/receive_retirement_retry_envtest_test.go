package attachment

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// Reuse the real API and TokenReview fixture. A sibling's authenticated commit
// races the target's CAS; the next single-consumer submission must reauthenticate
// the retained complete batch. Native absence is synthetic, not live cleanup.
func verifyRetirementCommitConflictRetry(t *testing.T, api kubernetes.Interface, stores []handover.Store, snapshots []handover.Snapshot, mesh *corev1.Secret, inventoryUID string, policies map[string]handover.PodReceiptAuthenticator, entries []handover.PodReceipt) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshots[0], stores[0].ClusterUID)
	must(err)
	target, err := NewReceiveRetirementReceiptScope(ctx, stores[0].ConfigMaps, stores[0].ClusterUID, inventoryUID, snapshots[0], mesh)
	must(err)
	injected := false
	writer := stores[0]
	writer.ConfigMaps = retirementInventoryWriter{ConfigMapInterface: stores[0].ConfigMaps, before: func() error {
		if injected {
			return nil
		}
		injected = true
		sibling, err := NewReceiveRetirementReceiptScope(ctx, stores[1].ConfigMaps, stores[1].ClusterUID, inventoryUID, snapshots[1], mesh)
		if err != nil {
			return err
		}
		batch := slices.Clone(entries)
		for i := range batch {
			batch[i].Receipt.RoundHash = sibling.Digest()
		}
		return RecordReceiveRetirementWithPodReceipts(ctx, api, stores[1], inventoryUID, sibling.Digest(), mesh, snapshots[1], batch, policies, time.Minute, func() error { return nil })
	}}
	resolutions, completions := 0, 0
	handler, err := handover.NewReceiptCollectionHandler(func(run context.Context, machine string) (handover.ReceiptCollection, error) {
		resolutions++
		source, err := ReadReceiveRetirementSet(run, api, writer, mesh.Namespace, mesh.Name, string(mesh.UID), machine)
		if err != nil {
			return handover.ReceiptCollection{}, err
		}
		collection, err := source.ReceiptCollection(run, api, writer, policies, time.Minute, func() error { return nil })
		if err != nil {
			return handover.ReceiptCollection{}, err
		}
		complete := collection.Complete
		collection.Complete = func(run context.Context, batch []handover.PodReceipt) error {
			completions++
			return complete(run, batch)
		}
		return collection, nil
	}, 1)
	must(err)
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	submit := func(entry handover.PodReceipt) int {
		t.Helper()
		bearer := entry.BearerToken
		entry.BearerToken = ""
		body, err := json.Marshal(handover.ReceiptRequest{MachineUID: plan.MachineUID, RoundHash: target.Digest(), Receipts: []handover.PodReceipt{entry}})
		must(err)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
		must(err)
		request.Header.Set("Authorization", "Bearer "+bearer)
		response, err := server.Client().Do(request)
		must(err)
		response.Body.Close()
		return response.StatusCode
	}
	if submit(entries[0]) != http.StatusAccepted || submit(entries[1]) != http.StatusConflict || !injected {
		t.Fatal("did not exercise the sibling commit conflict")
	}
	source, ok, err := ReadVerifiedReceiveRetirementCompletion(ctx, api, stores[0], mesh.Namespace, mesh.Name, string(mesh.UID), plan.MachineUID, inventoryUID, time.Minute)
	must(err)
	if ok || source.Metadata.UID != "" {
		t.Fatal("conflicting target write persisted completion")
	}
	if submit(entries[1]) != http.StatusNoContent || resolutions != 3 || completions != 2 {
		t.Fatal("single-consumer retry did not complete through fresh admission")
	}
	source, ok, err = ReadVerifiedReceiveRetirementCompletion(ctx, api, stores[0], mesh.Namespace, mesh.Name, string(mesh.UID), plan.MachineUID, inventoryUID, time.Minute)
	must(err)
	if !ok {
		t.Fatal("retried authenticated completion is not verifiable")
	}
	var completion receiveRetirementCompletion
	must(json.Unmarshal(source.Inventory.Publications[plan.MachineUID].RetirementCompletion, &completion))
	if len(completion.Receipts) != len(entries) {
		t.Fatal("retried completion lost consumers")
	}
	for _, receipt := range completion.Receipts {
		for _, entry := range entries {
			if receipt.NodeUID == entry.Receipt.NodeUID && !receipt.ObservedAt.Equal(entry.Receipt.ObservedAt) {
				t.Fatal("retry renewed an observation timestamp")
			}
		}
	}
}
