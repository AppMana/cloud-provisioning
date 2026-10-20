package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
	snapshot := completeReceiveTestJournal(t, store)
	var err error
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
	t.Run("retirement-live-recheck", func(t *testing.T) {
		// Real live-source reads; no native mutation. Recheck every reserved
		// journal and inventory without rereading their immutable ancestry.
		counted := &changingJournalReader{ConfigMapInterface: store.ConfigMaps, change: func() error { return nil }}
		reader := store
		reader.ConfigMaps = counted
		if err := recheckReceiveRetirement(ctx, api, reader, admitted, "site", site); err != nil {
			t.Fatal(err)
		}
		if counted.reads != len(admitted.Journals)+1 {
			t.Fatalf("expected live journal and inventory reads, got %d", counted.reads)
		}
		for _, fault := range []string{"inventory", "mesh", "node", "boot", "key", "read", "cancelled"} {
			t.Run(fault, func(t *testing.T) {
				candidate := admitted
				reader := store
				key := site
				runCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				switch fault {
				case "inventory":
					candidate.Metadata.ResourceVersion = "stale"
				case "mesh":
					candidate.MeshMetadata.ResourceVersion = "stale"
				case "node":
					candidate.NodeUID = "replacement"
				case "boot":
					candidate.BootID = "previous-boot"
				case "key":
					key, _ = wgtypes.GeneratePrivateKey()
				case "read":
					reader.ConfigMaps = &changingJournalReader{ConfigMapInterface: store.ConfigMaps, reads: 1, change: func() error { return errors.New("API unavailable") }}
				case "cancelled":
					cancel()
				}
				if err := recheckReceiveRetirement(runCtx, api, reader, candidate, "site", key); err == nil {
					t.Fatal("invalid source passed retirement recheck")
				}
			})
		}
	})
	t.Run("retirement-checkpoint", func(t *testing.T) {
		cfg := config{iface: "retiretest", nodeName: "site", privateKeyFile: filepath.Join(t.TempDir(), "key")}
		lock, err := lockReconciliation(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		persist := func(a admittedReceiveRetirement) error {
			return checkpointReceiveRetirementLocked(ctx, api, store, scope, cfg, site, lock.Name(), a)
		}
		// Already admitted immutable history must not be reconstructed just
		// to checkpoint intent. Budget one live read per journal plus inventory;
		// Node and mesh checks remain separate. This is not a latency guarantee.
		counted := &changingJournalReader{ConfigMapInterface: store.ConfigMaps, change: func() error { return nil }}
		reader := store
		reader.ConfigMaps = counted
		if err := checkpointReceiveRetirementLocked(ctx, api, reader, scope, cfg, site, lock.Name(), admitted); err != nil {
			t.Fatal(err)
		}
		if counted.reads > len(admitted.Journals)+1 {
			t.Fatalf("checkpoint reconstructed admitted history: %d ConfigMap reads", counted.reads)
		}
		if err := persist(admitted); err != nil {
			t.Fatal("retirement checkpoint retry failed", err)
		}
		path := lock.Name() + ".inventory.json"
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var saved receiveInventoryCheckpoint
		if err := json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.NodeUID != admitted.NodeUID || saved.BootID != admitted.BootID || len(saved.Inventory.Publications[scope.MachineUID].Retirement) == 0 {
			t.Fatal("checkpoint lost retirement identity")
		}
		journalPath := lock.Name() + "." + store.Name + ".json"
		journal, err := readReceiveJournalCheckpoint(journalPath, store, scope, cfg.nodeName, site)
		if err != nil || !journal.Snapshot.Frozen() || journal.NativeObservation != nil {
			t.Fatal("retirement checkpoint lost fence or invented native completion", err)
		}
		for _, name := range []string{path, journalPath} {
			info, err := os.Stat(name)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("retirement checkpoint is not private", err)
			}
		}
		stale := admitted
		stale.Metadata.ResourceVersion = "stale"
		if err := persist(stale); err == nil {
			t.Fatal("stale admission checkpointed")
		}
		for _, fault := range []string{"namespace", "mesh", "mesh-uid", "machine", "store", "journal-uid"} {
			changedScope, changedStore := scope, store
			switch fault {
			case "namespace":
				changedScope.Namespace = "different"
			case "mesh":
				changedScope.MeshName = "different"
			case "mesh-uid":
				changedScope.MeshUID = "different"
			case "machine":
				changedScope.MachineUID = "different"
			case "store":
				changedStore.Name = "different"
			case "journal-uid":
				changedStore.ExpectedUID = "different"
			}
			if err := checkpointReceiveRetirementLocked(ctx, api, changedStore, changedScope, cfg, site, lock.Name(), admitted); err == nil {
				t.Fatalf("checkpoint accepted changed %s scope", fault)
			}
		}
		if err := os.WriteFile(journalPath, []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := persist(admitted); err == nil {
			t.Fatal("corrupt prior history overwritten")
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(raw) {
			t.Fatal("failed checkpoint changed durable inventory", err)
		}
		if _, err := loadReceiveSet(ctx, api, store, scope, cfg.nodeName, site); err == nil {
			t.Fatal("serving reconciliation admitted a retiring inventory")
		}
	})
	verifyReceiveRetirementExecution(t, api, store, scope, site)
	wrong, _ := wgtypes.GeneratePrivateKey()
	if _, err := load(store, "site", wrong); err == nil {
		t.Fatal("changed local key admitted")
	}
	if _, err := load(store, "cloud", site); err == nil {
		t.Fatal("retiring node admitted as survivor")
	}
	t.Run("retirement-heartbeat", func(t *testing.T) {
		raced := store
		changed := false
		raced.ConfigMaps = &changingJournalReader{ConfigMapInterface: store.ConfigMaps, reads: -3, change: func() error {
			n, err := api.CoreV1().Nodes().Get(ctx, "site", metav1.GetOptions{})
			if err != nil {
				return err
			}
			for i := range n.Status.Conditions {
				n.Status.Conditions[i].LastHeartbeatTime = metav1.Now()
			}
			_, err = api.CoreV1().Nodes().UpdateStatus(ctx, n, metav1.UpdateOptions{})
			changed = err == nil
			return err
		}}
		if _, err := load(raced, "site", site); err != nil || !changed {
			t.Fatal("heartbeat prevented retirement admission", err)
		}
	})
	for _, fault := range []string{"mesh", "inventory", "journal", "node"} {
		t.Run("retirement-race-"+fault, func(t *testing.T) {
			raced := store
			// Shared admission reads index/journal and rechecks both first.
			// Inject on the fifth read, after local Node admission, when the
			// agent rechecks the journal again. Earlier Node changes belong
			// to the initial snapshot and are not a concurrent admission race.
			raced.ConfigMaps = &changingJournalReader{ConfigMapInterface: store.ConfigMaps, reads: -3, change: func() error {
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
	t.Run("reused-consumer-name", func(t *testing.T) {
		// Earlier subtests restore the active withdrawal. Install the same
		// synthetic historical release only for this replacement admission.
		seedReceiveFinalization(t, api, store, scope, false)
		source, err := attachment.ReadReceiveRetirementSet(ctx, api, store, scope.Namespace, scope.MeshName, scope.MeshUID, scope.MachineUID)
		if err != nil {
			t.Fatal(err)
		}
		if err := tunnel.ReleaseReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, source.Metadata, source.Mesh, source.Journals[scope.MachineUID], source.Inventory.Publications[scope.MachineUID], func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		verifyReleasedCheckpointNameReuse(t, api, store, scope, replacement, site, string(node.UID))
	})
}
