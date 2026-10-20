package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// Reuse the real-API joint fixture. Transport acceptance here is synthetic;
// authenticated completion/release is exercised by its native lifecycle test.
func verifyJointDispatchOnce(t *testing.T, api kubernetes.Interface, stores []handover.Store, mesh *corev1.Secret, cfg config, key wgtypes.Key, native receiveRetirementNative) {
	t.Helper()
	ctx := context.Background()
	delivered := make(chan handover.ReceiptRequest, len(stores))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cohort-token" {
			t.Error("missing request credential")
		}
		var request handover.ReceiptRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		delivered <- request
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	dir := t.TempDir()
	cfg.receiptURL = server.URL
	cfg.receiptCAFile = filepath.Join(dir, "ca")
	cfg.receiptTokenFile = filepath.Join(dir, "token")
	cfg.secretNamespace = mesh.Namespace
	cfg.secretName = mesh.Name
	cfg.receiptMeshName = mesh.Name
	for path, raw := range map[string][]byte{cfg.privateKeyFile: []byte(key.String()), cfg.receiptTokenFile: []byte("cohort-token"), cfg.receiptCAFile: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})} {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	phase := func(context.Context, tunnel.ReceiveTransitionPlan, handover.View, string, tunnel.ReceiveJournalRoutes, wgtypes.Key) error {
		return fmt.Errorf("retirement entered phase maintenance")
	}
	handled, err := dispatchReceivePhases(ctx, api, cfg, phase, phase, func(ctx context.Context, store handover.Store, scope receiveJournalScope, got wgtypes.Key, path string, member admittedReceiveRetirement) error {
		if got != key || scope.MachineUID != member.Withdrawal.MachineUID || store.Name != tunnel.ReceiveTransitionName(scope.MachineUID) {
			return fmt.Errorf("member dispatch lost identity")
		}
		calls = append(calls, scope.MachineUID)
		if native != nil {
			return native(ctx, path, member)
		}
		return nil
	})
	if !handled || err != nil {
		t.Fatalf("joint dispatch failed: %v", err)
	}
	var expected []string
	for _, store := range stores {
		snapshot, e := store.Load(ctx)
		if e != nil {
			t.Fatal(e)
		}
		plan, e := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
		if e != nil {
			t.Fatal(e)
		}
		expected = append(expected, plan.MachineUID)
	}
	slices.Sort(expected)
	if !slices.Equal(calls, expected) {
		t.Fatalf("cohort cleanup repeated or omitted: got %v want %v", calls, expected)
	}
	seen := map[string]bool{}
	var observed time.Time
	for range stores {
		select {
		case request := <-delivered:
			if !slices.Contains(expected, request.MachineUID) || seen[request.MachineUID] || len(request.Receipts) != 1 {
				t.Fatal("missing or duplicate member delivery")
			}
			seen[request.MachineUID] = true
			entry := request.Receipts[0]
			if entry.BearerToken != "" || (!observed.IsZero() && !entry.Receipt.ObservedAt.Equal(observed)) {
				t.Fatal("delivery changed observation time or leaked credentials")
			}
			observed = entry.Receipt.ObservedAt
			store := stores[0]
			store.Name = tunnel.ReceiveTransitionName(request.MachineUID)
			store.ExpectedUID = ""
			snapshot, e := store.Load(ctx)
			if e != nil {
				t.Fatal(e)
			}
			_, index, e := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
			if e != nil {
				t.Fatal(e)
			}
			binding, e := attachment.NewReceiveRetirementReceiptScope(ctx, store.ConfigMaps, store.ClusterUID, string(index.UID), snapshot, mesh)
			if e != nil {
				t.Fatal(e)
			}
			if request.RoundHash != binding.Digest() || entry.Receipt.RoundHash != binding.Digest() {
				t.Fatal("delivery lost member-specific binding")
			}
		default:
			t.Fatal("joint dispatch did not join all deliveries")
		}
	}
}
