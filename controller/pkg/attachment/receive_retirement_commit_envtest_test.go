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
	t.Run("admitted-collection-read-budget", func(t *testing.T) {
		verifyRetirementCollectionAdmission(t, api, store, mesh, w.MachineUID, map[string]handover.PodReceiptAuthenticator{string(node.UID): policy})
	})
	for _, fault := range []string{"missing-policy", "recheck", "inventory-race", "inventory-revision-race", "mesh-race", "reboot-after-authentication", "transport-inventory-race", "transport-commit-race", "none"} {
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
				case "inventory-race", "inventory-revision-race", "transport-inventory-race", "transport-commit-race":
					latest, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					original := latest.Annotations
					latest.Annotations = map[string]string{"race": fault}
					latest, err = maps.Update(ctx, latest, metav1.UpdateOptions{})
					if err == nil && fault == "inventory-revision-race" {
						// Restore identical authority while changing its revision.
						// Stable receipt bindings must not permit a stale commit.
						latest.Annotations = original
						_, err = maps.Update(ctx, latest, metav1.UpdateOptions{})
					}
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
					source, err := ReadReceiveRetirementSet(requestCtx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machine)
					if err != nil {
						return handover.ReceiptCollection{}, err
					}
					return source.ReceiptCollection(requestCtx, api, store, policies, time.Minute, recheck)
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
			commitBinding, err := receiveRetirementDigest("cluster", string(index.UID), scope.inventory.ResourceVersion, mesh, scope.intent)
			must(err)
			if completion.Version != receiveRetirementCompletionVersion || completion.InventoryVersion != scope.inventory.ResourceVersion || completion.InventoryHash != scope.inventoryHash || completion.MeshVersion != mesh.ResourceVersion || completion.Binding != commitBinding || len(completion.Receipts) != 1 || completion.Receipts[0].AgentPodUID != string(pod.UID) || completion.Receipts[0].ObservedBootID != entry.BootID {
				t.Fatal("lost authenticated completion identity")
			}
			if strings.Contains(string(record), entry.BearerToken) || len(v.MachineUIDs) != 1 || v.Publications[w.MachineUID].JournalUID != string(journal.UID) {
				t.Fatal("completion persisted credential or released reservation")
			}
			verified, err := VerifyReceiveRetirementCompletion(ctx, api, store, ns, mesh.Name, string(mesh.UID), w.MachineUID, string(index.UID), time.Minute)
			must(err)
			if !verified {
				t.Fatal("authenticated completion did not survive its own inventory write")
			}
			// Legacy records keep their revision-bound receipt semantics.
			// A newer reader must not reinterpret retained evidence.
			for _, legacyVersion := range []int{2, 3} {
				legacy := completion
				legacy.Receipts = append([]handover.Receipt(nil), completion.Receipts...)
				for i := range legacy.Receipts {
					legacy.Receipts[i].RoundHash = commitBinding
				}
				legacy.Version = legacyVersion
				legacy.InventoryHash, err = receiveRetirementInventoryHash(v, w.MachineUID, legacyVersion)
				must(err)
				legacyRaw, err := json.Marshal(legacy)
				must(err)
				currentInventory, currentIndex, err := tunnel.ReadReceiveInventory(ctx, maps, "cluster", ns, mesh.Name, string(mesh.UID))
				must(err)
				publication := currentInventory.Publications[w.MachineUID]
				publication.RetirementCompletion = legacyRaw
				currentInventory.Publications[w.MachineUID] = publication
				encoded, err := json.Marshal(currentInventory)
				must(err)
				currentIndex.Data["inventory.json"] = string(encoded)
				currentIndex, err = maps.Update(ctx, currentIndex, metav1.UpdateOptions{})
				must(err)
				legacyOK, err := VerifyReceiveRetirementCompletion(ctx, api, store, ns, mesh.Name, string(mesh.UID), w.MachineUID, string(index.UID), time.Minute)
				must(err)
				if !legacyOK {
					t.Fatal("valid legacy completion rejected")
				}
				publication.RetirementCompletion = record
				currentInventory.Publications[w.MachineUID] = publication
				encoded, err = json.Marshal(currentInventory)
				must(err)
				currentIndex.Data["inventory.json"] = string(encoded)
				_, err = maps.Update(ctx, currentIndex, metav1.UpdateOptions{})
				must(err)
			}
			for _, damage := range []string{"version", "inventory-version", "inventory-hash", "mesh-version", "binding", "legacy-receipts", "missing", "duplicate", "container", "stale", "phase"} {
				t.Run("readback/"+damage, func(t *testing.T) {
					var changed receiveRetirementCompletion
					must(json.Unmarshal(record, &changed))
					switch damage {
					case "version":
						changed.Version = 1
					case "inventory-version":
						changed.InventoryVersion = "different"
					case "inventory-hash":
						changed.InventoryHash = "different"
					case "mesh-version":
						changed.MeshVersion = "different"
					case "binding":
						changed.Binding = "different"
					case "legacy-receipts":
						changed.Version = 3
					case "missing":
						changed.Receipts = nil
					case "duplicate":
						changed.Receipts = append(changed.Receipts, changed.Receipts[0])
					case "container":
						changed.Receipts[0].AgentContainerID = "replacement"
					case "stale":
						changed.Receipts[0].ObservedAt = time.Now().Add(-time.Hour)
					case "phase":
						changed.Receipts[0].DeviceID = "receiver"
					}
					damaged, err := json.Marshal(changed)
					must(err)
					current, object, err := tunnel.ReadReceiveInventory(ctx, maps, "cluster", ns, mesh.Name, string(mesh.UID))
					must(err)
					publication := current.Publications[w.MachineUID]
					publication.RetirementCompletion = damaged
					current.Publications[w.MachineUID] = publication
					raw, err := json.Marshal(current)
					must(err)
					object.Data["inventory.json"] = string(raw)
					_, err = maps.Update(ctx, object, metav1.UpdateOptions{})
					must(err)
					if ok, err := VerifyReceiveRetirementCompletion(ctx, api, store, ns, mesh.Name, string(mesh.UID), w.MachineUID, string(index.UID), time.Minute); err == nil || ok {
						t.Fatal("invalid completion authorized deletion")
					}
					current.Publications[w.MachineUID] = v.Publications[w.MachineUID]
					raw, err = json.Marshal(current)
					must(err)
					object, err = maps.Get(ctx, index.Name, metav1.GetOptions{})
					must(err)
					object.Data["inventory.json"] = string(raw)
					_, err = maps.Update(ctx, object, metav1.UpdateOptions{})
					must(err)
				})
			}
			currentNode, err := api.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
			must(err)
			oldBoot := currentNode.Status.NodeInfo.BootID
			currentNode.Status.NodeInfo.BootID = "reboot-after-completion"
			currentNode, err = api.CoreV1().Nodes().UpdateStatus(ctx, currentNode, metav1.UpdateOptions{})
			must(err)
			if ok, err := VerifyReceiveRetirementCompletion(ctx, api, store, ns, mesh.Name, string(mesh.UID), w.MachineUID, string(index.UID), time.Minute); err == nil || ok {
				t.Fatal("reboot reused old completion")
			}
			currentNode.Status.NodeInfo.BootID = oldBoot
			_, err = api.CoreV1().Nodes().UpdateStatus(ctx, currentNode, metav1.UpdateOptions{})
			must(err)
			expired := entry
			expired.Receipt.ObservedAt = time.Now().Add(-2 * time.Minute)
			if err := RecordReceiveRetirementWithPodReceipts(ctx, api, store, string(index.UID), scope.Digest(), mesh, snapshot, []handover.PodReceipt{expired}, policies, time.Minute, func() error { return nil }); err == nil {
				t.Fatal("expired observation reused after completion write")
			}
			freshScope, err := NewReceiveRetirementReceiptScope(ctx, maps, "cluster", string(index.UID), snapshot, mesh)
			must(err)
			freshEntry := entry
			freshEntry.Receipt.RoundHash = freshScope.Digest()
			freshEntry.Receipt.ObservedAt = time.Now()
			denied := freshEntry
			denied.BearerToken = "invalid-token"
			if err := RecordReceiveRetirementWithPodReceipts(ctx, api, store, string(index.UID), freshScope.Digest(), mesh, snapshot, []handover.PodReceipt{denied}, policies, time.Minute, func() error { return nil }); err == nil {
				t.Fatal("unauthenticated refresh accepted")
			}
			must(RecordReceiveRetirementWithPodReceipts(ctx, api, store, string(index.UID), freshScope.Digest(), mesh, snapshot, []handover.PodReceipt{freshEntry}, policies, time.Minute, func() error { return nil }))
			ok, err := VerifyReceiveRetirementCompletion(ctx, api, store, ns, mesh.Name, string(mesh.UID), w.MachineUID, string(index.UID), time.Minute)
			must(err)
			if !ok {
				t.Fatal("authenticated refreshed completion not verifiable")
			}
		})
	}
}
