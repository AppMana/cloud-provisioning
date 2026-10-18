package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Uses the existing isolated API fixture after its ordinary admission checks.
// Phase receipts are synthetic. No native callback, cleanup or retirement
// receipt is produced; disappearance of the cloud objects proves only that
// survivor admission does not depend on the retiring worker remaining alive.
func verifyReceiveRetirementAdmission(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, site wgtypes.Key) {
	t.Helper()
	ctx := context.Background()
	snapshot, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for steps := 0; ; steps++ {
		if steps > 10 {
			t.Fatal("fixture transition did not complete")
		}
		view, err := snapshot.Transition().View()
		if err != nil {
			t.Fatal(err)
		}
		if view.Complete {
			break
		}
		at := view.Round.IssuedAt.Add(time.Second)
		if view.Round.Phase == handover.Drain {
			at = view.DrainUntil
		}
		hash, err := view.Round.Digest()
		if err != nil {
			t.Fatal(err)
		}
		var proofs []handover.AuthenticatedReceipt
		for _, m := range view.Round.Members {
			proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: hash, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: at}})
		}
		next, err := snapshot.Transition().Advance(at, proofs)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err = store.Commit(ctx, snapshot, next)
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err = store.FreezeCompleted(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	if err != nil {
		t.Fatal(err)
	}
	mesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mesh.Data[tunnel.PeerPublicKeyPrefix+plan.MachineName] = []byte(plan.RootPublicKey)
	mesh, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, mesh, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	if err != nil {
		t.Fatal(err)
	}
	// This older admission fixture uses legacy reservations. Bind its exact live
	// journal explicitly; production publication does this before execution.
	if inventory.Publications == nil {
		inventory.Publications = map[string]tunnel.ReceiveJournalPublication{}
	}
	inventory.Publications[scope.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(snapshot.Metadata().UID)}
	raw, _ := json.Marshal(inventory)
	index.Data["inventory.json"] = string(raw)
	index, err = store.ConfigMaps.Update(ctx, index, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w := attachment.RemoteWithdrawalIntent{MeshNamespace: scope.Namespace, MeshName: scope.MeshName, MeshUID: scope.MeshUID, SourceVersion: mesh.ResourceVersion, MachineName: plan.MachineName, MachineUID: plan.MachineUID, PublicKey: plan.RootPublicKey, DrainIntent: "test/withdrawal"}
	for public, s := range plan.Sites {
		w.Consumers = append(w.Consumers, attachment.ConsumerTarget{Site: true, NodeName: s.NodeName, NodeUID: s.NodeUID, PublicKey: public})
	}
	if err := w.ReserveReceiveWithdrawal(ctx, store.ConfigMaps, store.ClusterUID, string(index.UID), mesh, snapshot); err != nil {
		t.Fatal(err)
	}
	load := func(s handover.Store, name string, key wgtypes.Key) (admittedReceiveRetirement, error) {
		return loadReceiveRetirement(ctx, api, s, scope, name, key)
	}
	if _, err := load(store, "site", site); err == nil {
		t.Fatal("admitted retirement before withdrawal publication")
	}
	mesh.Data, _, err = tunnel.WithdrawRemotePeer(mesh.Data, plan.MachineName, plan.RootPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.CoreV1().Secrets(scope.Namespace).Update(ctx, mesh, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := api.CoreV1().Nodes().Delete(ctx, plan.CloudNodeName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := api.CoreV1().Secrets(scope.Namespace).Delete(ctx, plan.AdoptionName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	admitted, err := load(store, "site", site)
	if err != nil || admitted.NodeUID != plan.Sites[site.PublicKey().String()].NodeUID || len(admitted.Plans) != len(inventory.MachineUIDs) || len(admitted.Withdrawal.Consumers) != len(w.Consumers) {
		t.Fatal("lost survivor or reservation after cloud deletion", err)
	}
	wrong, _ := wgtypes.GeneratePrivateKey()
	if _, err := load(store, "site", wrong); err == nil {
		t.Fatal("changed local key admitted")
	}
	if _, err := load(store, "cloud", site); err == nil {
		t.Fatal("retiring node admitted as survivor")
	}
	for _, fault := range []string{"mesh", "inventory", "journal", "node"} {
		t.Run("retirement-race-"+fault, func(t *testing.T) {
			raced := store
			// First read is the index, second the journal. Inject on the third
			// read, after local Node admission, when the journal is rechecked.
			raced.ConfigMaps = &changingJournalReader{ConfigMapInterface: store.ConfigMaps, reads: -1, change: func() error {
				switch fault {
				case "mesh":
					m, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
					if err != nil {
						return err
					}
					m.Annotations = map[string]string{"retirement-race": fault}
					_, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, m, metav1.UpdateOptions{})
					return err
				case "node":
					n, err := api.CoreV1().Nodes().Get(ctx, "site", metav1.GetOptions{})
					if err != nil {
						return err
					}
					n.Annotations = map[string]string{"retirement-race": fault}
					_, err = api.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
					return err
				default:
					name := store.Name
					if fault == "inventory" {
						name = index.Name
					}
					object, err := store.ConfigMaps.Get(ctx, name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					object.Annotations = map[string]string{"retirement-race": fault}
					_, err = store.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{})
					return err
				}
			}}
			if _, err := load(raced, "site", site); err == nil {
				t.Fatal("changed retirement source admitted")
			}
		})
	}
	t.Run("retirement-missing-reserved-journal", func(t *testing.T) {
		_, original, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			current, err := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			current.Data = original.Data
			if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
				t.Error(err)
			}
		})
		currentMesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := tunnel.ReserveReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, currentMesh, "unavailable-worker", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := load(store, "site", site); err == nil {
			t.Fatal("missing reserved worker journal silently omitted")
		}
	})
	// Same-name replacement must not inherit the departed survivor's cleanup
	// authority, even with matching key material, boot status and readiness.
	node, err := api.CoreV1().Nodes().Get(ctx, "site", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.CoreV1().Nodes().Delete(ctx, "site", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	replacement, err := api.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "site"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replacement.Status = node.Status
	if _, err := api.CoreV1().Nodes().UpdateStatus(ctx, replacement, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := load(store, "site", site); err == nil {
		t.Fatal("replacement survivor inherited retirement authority")
	}
}
