package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Two withdrawn histories and one serving journal, using real API storage.
// Terminal proofs are synthetic; absent owner objects do not establish actual
// deletion, native absence, authenticated completion or live admission latency.
func verifySharedSurvivorRetirements(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, retiring handover.Store, plan tunnel.ReceiveTransitionPlan, withdrawal attachment.RemoteWithdrawalIntent, key, cloudKey wgtypes.Key) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	must(err)
	originalIndex := index.DeepCopy()
	mesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
	must(err)
	originalMesh := mesh.DeepCopy()
	thirdKey, err := wgtypes.GeneratePrivateKey()
	must(err)
	raw, err := json.Marshal(plan)
	must(err)
	var third tunnel.ReceiveTransitionPlan
	must(json.Unmarshal(raw, &third))
	third.MachineName, third.MachineUID = "worker-three", "third-machine-uid"
	third.CloudNodeName, third.CloudNodeUID = "cloud-three", "third-cloud-uid"
	third.AdoptionName, third.AdoptionUID = tunnel.AdoptionSecretName(third.MachineName), "third-adoption-uid"
	third.RootPublicKey = thirdKey.PublicKey().String()
	binding := third.Bindings[key.PublicKey().String()]
	binding.PublicKey = receiverKey(thirdKey, scope.MeshUID, key.PublicKey().String()).PublicKey().String()
	third.Bindings[key.PublicKey().String()] = binding
	third.SiteDesired = tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: third.RootPublicKey, Remote: true, Endpoint: "198.51.100.3:51820", WGAllowedIPs: []string{"172.29.0.3/32"}}}}
	site := third.Sites[key.PublicKey().String()]
	site.Client, err = tunnel.PlanSiteReceiveClient(scope.MeshUID, key.PublicKey().String(), third.RootPublicKey, binding, third.SiteDesired, third.SiteDesired)
	must(err)
	third.Sites[key.PublicKey().String()] = site
	thirdStore := store
	thirdStore.Name, thirdStore.ExpectedUID = tunnel.ReceiveTransitionName(third.MachineUID), ""
	must(tunnel.ReserveReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, third.MachineUID, store.OwnerReferences))
	t.Cleanup(func() {
		current, err := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
		must(err)
		current.Data = originalIndex.Data
		_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
		must(err)
		must(store.ConfigMaps.Delete(ctx, thirdStore.Name, metav1.DeleteOptions{}))
		currentMesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
		must(err)
		currentMesh.Data = originalMesh.Data
		_, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, currentMesh, metav1.UpdateOptions{})
		must(err)
	})
	raw, err = json.Marshal(third)
	must(err)
	spec, err := tunnel.ReceiveTransitionSpec(third, store.ClusterUID, fmt.Sprintf("%x", sha256.Sum256(raw)))
	must(err)
	transition, err := handover.NewTransition(spec, time.Now(), handover.Policy{RoundLifetime: 45 * time.Second, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
	must(err)
	_, err = thirdStore.CreateWithPlan(ctx, transition, raw)
	must(err)
	snapshot := completeReceiveTestJournal(t, thirdStore)
	snapshot, err = thirdStore.FreezeCompleted(ctx, snapshot)
	must(err)
	inventory, index, err = tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	must(err)
	inventory.Publications[third.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(snapshot.Metadata().UID)}
	raw, err = json.Marshal(inventory)
	must(err)
	index.Data["inventory.json"] = string(raw)
	index, err = store.ConfigMaps.Update(ctx, index, metav1.UpdateOptions{})
	must(err)
	mesh.Data[tunnel.PeerPublicKeyPrefix+third.MachineName] = []byte(third.RootPublicKey)
	mesh, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	withdrawal.MachineName, withdrawal.MachineUID, withdrawal.PublicKey, withdrawal.SourceVersion = third.MachineName, third.MachineUID, third.RootPublicKey, mesh.ResourceVersion
	must(withdrawal.ReserveReceiveWithdrawal(ctx, store.ConfigMaps, store.ClusterUID, string(index.UID), mesh, snapshot))
	mesh.Data, _, err = tunnel.WithdrawRemotePeer(mesh.Data, third.MachineName, third.RootPublicKey)
	must(err)
	_, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	reads := 0
	reader := store
	reader.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
		if name == retiring.Name {
			reads++
		}
		return nil
	}}
	admitted, err := loadReceiveSet(ctx, api, reader, scope, "site", key)
	must(err)
	if len(admitted.Retirements) != 2 || len(admitted.Journals) != 3 || len(admitted.Routes.Plans) != 1 {
		t.Fatal("shared admission lost reserved or serving membership")
	}
	if reads > 5 {
		t.Fatalf("duplicate full-set admission: %d retained root reads, budget 5", reads)
	}
	serving, err := store.Load(ctx)
	must(err)
	servingPlan, err := tunnel.DecodeReceiveTransition(serving, store.ClusterUID)
	must(err)
	for _, fault := range []string{"consumer-replaced", "consumer-omitted", "malformed", "source-race"} {
		t.Run(fault, func(t *testing.T) {
			inventory, original, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
			must(err)
			t.Cleanup(func() {
				current, err := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
				must(err)
				current.Data, current.Annotations = original.Data, original.Annotations
				_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
				must(err)
			})
			reader := store
			if fault == "source-race" {
				reads := 0
				reader.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
					if name != thirdStore.Name {
						return nil
					}
					reads++
					if reads != 4 {
						return nil
					}
					current, err := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if current.Annotations == nil {
						current.Annotations = map[string]string{}
					}
					current.Annotations["test-shared-race"] = "changed"
					_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
					return err
				}}
				if _, err := loadReceiveSet(ctx, api, reader, scope, servingPlan.CloudNodeName, cloudKey); err == nil || reads < 4 {
					t.Fatal("shared admission missed source mutation", err)
				}
				return
			}
			publication := inventory.Publications[third.MachineUID]
			if fault == "malformed" {
				publication.Retirement = json.RawMessage(`{}`)
			} else {
				var record struct {
					Version       int                               `json:"version"`
					JournalSHA256 string                            `json:"journalSHA256"`
					Withdrawal    attachment.RemoteWithdrawalIntent `json:"withdrawal"`
				}
				must(json.Unmarshal(publication.Retirement, &record))
				var consumers []attachment.ConsumerTarget
				changed := false
				for _, consumer := range record.Withdrawal.Consumers {
					if consumer.NodeName == servingPlan.CloudNodeName {
						changed = true
						if fault == "consumer-omitted" {
							continue
						}
						consumer.NodeUID = "replacement-cloud-uid"
					}
					consumers = append(consumers, consumer)
				}
				if !changed {
					t.Fatal("missing cloud consumer fixture")
				}
				record.Withdrawal.Consumers = consumers
				publication.Retirement, err = json.Marshal(record)
				must(err)
			}
			inventory.Publications[third.MachineUID] = publication
			raw, err := json.Marshal(inventory)
			must(err)
			current := original.DeepCopy()
			current.Data["inventory.json"] = string(raw)
			_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
			must(err)
			if _, err := loadReceiveSet(ctx, api, reader, scope, servingPlan.CloudNodeName, cloudKey); err == nil {
				t.Fatal("invalid sibling withdrawal inherited admission")
			}
		})
	}
}
