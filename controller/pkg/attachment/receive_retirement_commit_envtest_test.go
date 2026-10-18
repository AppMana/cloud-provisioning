package attachment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Called from the real TokenRequest/TokenReview fixture. Transport history and
// native absence are synthetic; this proves authenticated persistence, including
// TLS collection. It does not qualify live deletion or reservation release.
func verifyRetirementAuthenticatedCommit(t *testing.T, api kubernetes.Interface, policy handover.PodReceiptAuthenticator, node *corev1.Node, pod *corev1.Pod, submission handover.PodReceipt) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ns := policy.Namespace
	mesh, err := api.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh"}}, metav1.CreateOptions{})
	must(err)
	w, fixture, inventory, _ := retirementFixtureForConsumer(t, "forward", true, string(mesh.UID), ns, node.Name, string(node.UID))
	mesh.Data = map[string][]byte{tunnel.NodePublicKeyPrefix + node.Name: []byte(w.Consumers[0].PublicKey), tunnel.PeerPublicKeyPrefix + w.MachineName: []byte(w.PublicKey)}
	mesh, err = api.CoreV1().Secrets(ns).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	w.SourceVersion = mesh.ResourceVersion
	raw, err := fixture.Checkpoint()
	must(err)
	var checkpoint struct {
		Object corev1.ConfigMap `json:"object"`
	}
	must(json.Unmarshal(raw, &checkpoint))
	checkpoint.Object.UID, checkpoint.Object.ResourceVersion = "", ""
	maps := api.CoreV1().ConfigMaps(ns)
	journal, err := maps.Create(ctx, &checkpoint.Object, metav1.CreateOptions{})
	must(err)
	store := handover.Store{ConfigMaps: maps, ClusterUID: "cluster", Name: journal.Name, ExpectedUID: string(journal.UID)}
	snapshot, err := store.Load(ctx)
	must(err)
	inventory.Publications[w.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(journal.UID)}
	raw, err = json.Marshal(inventory)
	must(err)
	index, err := maps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveInventoryName(w.MeshUID)}, Data: map[string]string{"inventory.json": string(raw)}}, metav1.CreateOptions{})
	must(err)
	must(w.ReserveReceiveWithdrawal(ctx, maps, "cluster", string(index.UID), mesh, snapshot))
	mesh.Data, _, err = tunnel.WithdrawRemotePeer(mesh.Data, w.MachineName, w.PublicKey)
	must(err)
	mesh, err = api.CoreV1().Secrets(ns).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	for _, fault := range []string{"missing-policy", "recheck", "inventory-race", "mesh-race", "reboot-after-authentication", "transport-inventory-race", "transport-commit-race", "none"} {
		t.Run("authenticated-completion/"+fault, func(t *testing.T) {
			scope, err := NewReceiveRetirementReceiptScope(ctx, maps, "cluster", string(index.UID), snapshot, mesh)
			must(err)
			entry := submission
			entry.Receipt.RoundHash = scope.Digest()
			entry.Receipt.ObservedAt = time.Now()
			policies := map[string]handover.PodReceiptAuthenticator{string(node.UID): policy}
			if fault == "missing-policy" {
				policies = nil
			}
			checks := 0
			recheck := func() error {
				checks++
				at := 2
				if fault == "transport-inventory-race" {
					at = 3
				}
				if fault == "transport-commit-race" {
					at = 5
				}
				if checks != at {
					return nil
				}
				switch fault {
				case "recheck":
					return errors.New("source admission changed")
				case "inventory-race", "transport-inventory-race", "transport-commit-race":
					latest, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					latest.Annotations = map[string]string{"race": fault}
					_, err = maps.Update(ctx, latest, metav1.UpdateOptions{})
					return err
				case "mesh-race":
					changed := mesh.DeepCopy()
					changed.Annotations = map[string]string{"race": "changed"}
					updated, e := api.CoreV1().Secrets(ns).Update(ctx, changed, metav1.UpdateOptions{})
					if e == nil {
						mesh = updated
					}
					return e
				case "reboot-after-authentication":
					node.Status.NodeInfo.BootID = "raced-after-authentication"
					node, err = api.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
					return err
				}
				return nil
			}

			if fault == "none" || strings.HasPrefix(fault, "transport-") {
				// The resolver constructs fresh admission for every request. A source
				// revision changed after partial authentication must leave no completion.
				handler, e := handover.NewReceiptCollectionHandler(func(requestCtx context.Context, machine string) (handover.ReceiptCollection, error) {
					if machine != w.MachineUID {
						return handover.ReceiptCollection{}, fmt.Errorf("unknown machine")
					}
					return NewReceiveRetirementReceiptCollection(requestCtx, api, store, string(index.UID), mesh, snapshot, policies, time.Minute, recheck)
				}, 1)
				must(e)
				server := httptest.NewTLSServer(handler)
				defer server.Close()
				public := entry
				public.BearerToken = ""
				body, e := json.Marshal(handover.ReceiptRequest{MachineUID: w.MachineUID, RoundHash: scope.Digest(), Receipts: []handover.PodReceipt{public}})
				must(e)
				request, e := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
				must(e)
				request.Header.Set("Authorization", "Bearer "+entry.BearerToken)
				response, e := server.Client().Do(request)
				if e != nil {
					t.Fatal("TLS retirement submission failed")
				}
				response.Body.Close()
				err = nil
				if response.StatusCode != http.StatusNoContent {
					err = fmt.Errorf("retirement HTTP status %d", response.StatusCode)
				}
			} else {
				err = RecordReceiveRetirementWithPodReceipts(ctx, api, store, string(index.UID), scope.Digest(), mesh, snapshot, []handover.PodReceipt{entry}, policies, time.Minute, recheck)
			}
			v, _, readErr := tunnel.ReadReceiveInventory(ctx, maps, "cluster", ns, mesh.Name, string(mesh.UID))
			must(readErr)
			if fault == "reboot-after-authentication" {
				node.Status.NodeInfo.BootID = entry.BootID
				node, readErr = api.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
				must(readErr)
			}
			record := v.Publications[w.MachineUID].RetirementCompletion
			if fault != "none" {
				if err == nil || len(record) != 0 {
					t.Fatal("unsafe completion persisted", err)
				}
				return
			}
			must(err)
			var completion receiveRetirementCompletion
			must(json.Unmarshal(record, &completion))
			if completion.Version != 1 || completion.Binding != scope.Digest() || len(completion.Receipts) != 1 || completion.Receipts[0].AgentPodUID != string(pod.UID) || completion.Receipts[0].ObservedBootID != entry.BootID {
				t.Fatal("lost authenticated completion identity")
			}
			if strings.Contains(string(record), entry.BearerToken) || len(v.MachineUIDs) != 1 || v.Publications[w.MachineUID].JournalUID != string(journal.UID) {
				t.Fatal("completion persisted credential or released reservation")
			}
			if err := RecordReceiveRetirementWithPodReceipts(ctx, api, store, string(index.UID), scope.Digest(), mesh, snapshot, []handover.PodReceipt{entry}, policies, time.Minute, func() error { return nil }); err == nil {
				t.Fatal("stale binding reused after completion write")
			}
		})
	}
}
