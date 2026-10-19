package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Real API storage and synthetic phase proofs; the native callback checks
// serving intent only. This does not establish live forwarding or reboot time.
func verifySurvivorReceiveAdmission(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, retiring handover.Store, key, cloudKey wgtypes.Key) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot := completeReceiveTestJournal(t, retiring)
	var err error
	snapshot, err = retiring.FreezeCompleted(ctx, snapshot)
	must(err)
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	must(err)
	mesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
	must(err)
	mesh.Data[tunnel.PeerPublicKeyPrefix+plan.MachineName] = []byte(plan.RootPublicKey)
	mesh, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	must(err)
	if inventory.Publications == nil {
		inventory.Publications = map[string]tunnel.ReceiveJournalPublication{}
	}
	inventory.Publications[plan.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(snapshot.Metadata().UID)}
	raw, err := json.Marshal(inventory)
	must(err)
	index.Data["inventory.json"] = string(raw)
	index, err = store.ConfigMaps.Update(ctx, index, metav1.UpdateOptions{})
	must(err)
	w := attachment.RemoteWithdrawalIntent{MeshNamespace: scope.Namespace, MeshName: scope.MeshName, MeshUID: scope.MeshUID, SourceVersion: mesh.ResourceVersion, MachineName: plan.MachineName, MachineUID: plan.MachineUID, PublicKey: plan.RootPublicKey, DrainIntent: "test/delete"}
	for public, site := range plan.Sites {
		w.Consumers = append(w.Consumers, attachment.ConsumerTarget{Site: true, NodeName: site.NodeName, NodeUID: site.NodeUID, PublicKey: public})
	}
	servingSnapshot, err := store.Load(ctx)
	must(err)
	servingPlan, err := tunnel.DecodeReceiveTransition(servingSnapshot, store.ClusterUID)
	must(err)
	w.Consumers = append(w.Consumers, attachment.ConsumerTarget{NodeName: servingPlan.CloudNodeName, NodeUID: servingPlan.CloudNodeUID, PublicKey: servingPlan.RootPublicKey, MachineName: servingPlan.MachineName, SecretUID: servingPlan.AdoptionUID, TunnelAddress: "172.29.0.1"})
	must(w.ReserveReceiveWithdrawal(ctx, store.ConfigMaps, store.ClusterUID, string(index.UID), mesh, snapshot))
	cfg := config{iface: "survivor", nodeName: "site", privateKeyFile: filepath.Join(t.TempDir(), "key")}
	if _, err := loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key); err == nil {
		t.Fatal("unpublished retirement admitted as withdrawn")
	}
	mesh.Data, _, err = tunnel.WithdrawRemotePeer(mesh.Data, plan.MachineName, plan.RootPublicKey)
	must(err)
	mesh, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	must(api.CoreV1().Nodes().Delete(ctx, plan.CloudNodeName, metav1.DeleteOptions{}))
	must(api.CoreV1().Secrets(scope.Namespace).Delete(ctx, plan.AdoptionName, metav1.DeleteOptions{}))
	t.Run("retained-history-read-budget", func(t *testing.T) {
		reads := 0
		reader := store
		reader.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
			if name == retiring.Name {
				reads++
			}
			return nil
		}}
		_, err := loadReceiveSet(ctx, api, reader, scope, cfg.nodeName, key)
		must(err)
		// Initial admission reads and fences the retained journal three times.
		// The final boundary needs one current read, not another history load.
		// This fixture has no successor ancestry and proves a read budget only.
		if reads > 4 {
			t.Fatalf("survivor boundary reconstructed retained history: %d reads, budget 4", reads)
		}
	})
	t.Run("inventory-changed-at-final-boundary", func(t *testing.T) {
		original, err := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
		must(err)
		t.Cleanup(func() {
			current, err := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
			must(err)
			current.Annotations = original.Annotations
			_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
			must(err)
		})
		reads := 0
		reader := store
		reader.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
			if name != retiring.Name {
				return nil
			}
			reads++
			if reads != 4 {
				return nil
			}
			current, err := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if current.Annotations == nil {
				current.Annotations = map[string]string{}
			}
			current.Annotations["test-boundary-race"] = "changed"
			_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
			return err
		}}
		if _, err := loadReceiveSet(ctx, api, reader, scope, cfg.nodeName, key); err == nil {
			t.Fatal("admitted inventory changed at final survivor boundary")
		}
		if reads < 4 {
			t.Fatal("did not exercise final survivor boundary")
		}
	})
	t.Run("survivor-live-set-guard", func(t *testing.T) {
		admitted, err := loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key)
		must(err)
		reads := 0
		reader := store
		reader.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
			if name == retiring.Name {
				reads++
			}
			return nil
		}}
		must(recheckReceiveSet(ctx, api, reader, scope, cfg.nodeName, key, admitted))
		if reads != 1 {
			t.Fatalf("guard reloaded retained history: %d reads, want one live root read", reads)
		}
		t.Run("inventory-race", func(t *testing.T) {
			original, err := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
			must(err)
			t.Cleanup(func() {
				current, err := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
				must(err)
				current.Annotations = original.Annotations
				_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
				must(err)
			})
			changed := false
			racing := store
			racing.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
				if name != retiring.Name || changed {
					return nil
				}
				changed = true
				current, err := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if current.Annotations == nil {
					current.Annotations = map[string]string{}
				}
				current.Annotations["test-guard-race"] = "changed"
				_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
				return err
			}}
			if err := recheckReceiveSet(ctx, api, racing, scope, cfg.nodeName, key, admitted); err == nil || !changed {
				t.Fatal("concurrent inventory mutation escaped guard", err)
			}
		})
		admitted, err = loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key)
		must(err)
		for _, fault := range []string{"inventory", "node", "boot", "scope", "key", "cancelled"} {
			t.Run(fault, func(t *testing.T) {
				candidate := admitted
				candidate.Local = make(map[string]admittedReceiveJournal, len(admitted.Local))
				for uid, member := range admitted.Local {
					candidate.Local[uid] = member
				}
				member := candidate.Local[scope.MachineUID]
				localScope, localKey := scope, key
				runCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				switch fault {
				case "inventory":
					candidate.Metadata.ResourceVersion = "stale"
				case "node":
					member.NodeUID = "replacement"
				case "boot":
					member.BootID = "previous-boot"
				case "scope":
					localScope.MeshUID = "replacement"
				case "key":
					localKey, err = wgtypes.GeneratePrivateKey()
					must(err)
				case "cancelled":
					cancel()
				}
				candidate.Local[scope.MachineUID] = member
				if err := recheckReceiveSet(runCtx, api, store, localScope, cfg.nodeName, localKey, candidate); err == nil {
					t.Fatal("changed authority passed live-set guard")
				}
			})
		}
	})
	t.Run("cloud-survivor", func(t *testing.T) {
		cloudCfg := config{iface: "cloudsurvivor", nodeName: servingPlan.CloudNodeName, privateKeyFile: filepath.Join(t.TempDir(), "key")}
		invoked := false
		receipts, err := executeReceivePhase(ctx, api, store, scope, cloudCfg, cloudKey, "container", func(_ context.Context, p tunnel.ReceiveTransitionPlan, _ handover.View, uid string, routes tunnel.ReceiveJournalRoutes) error {
			invoked = true
			if uid != servingPlan.CloudNodeUID || p.MachineUID != scope.MachineUID || len(routes.Plans) != 1 {
				t.Fatal("cloud survivor route identity changed")
			}
			return nil
		})
		must(err)
		if !invoked || len(receipts) == 0 {
			t.Fatal("cloud survivor did not execute")
		}
	})
	calls := 0
	native := func(_ context.Context, p tunnel.ReceiveTransitionPlan, _ handover.View, _ string, routes tunnel.ReceiveJournalRoutes) error {
		calls++
		if p.MachineUID != scope.MachineUID || len(routes.Plans) != 1 || routes.Plans[plan.MachineUID].MachineUID != "" {
			t.Fatal("restoration revived withdrawn journal")
		}
		return nil
	}
	receipts, err := executeReceivePhase(ctx, api, store, scope, cfg, key, "container", native)
	must(err)
	if calls != 1 || len(receipts) != 1 {
		t.Fatal("survivor did not execute")
	}
	lockPath := filepath.Join(filepath.Dir(cfg.privateKeyFile), cfg.iface+".reconcile.lock")
	for _, name := range []string{store.Name, retiring.Name} {
		if _, err := os.Stat(lockPath + "." + name + ".json"); err != nil {
			t.Fatal("lost retained checkpoint", err)
		}
	}
	saved, err := os.ReadFile(lockPath + ".inventory.json")
	must(err)
	var checkpoint receiveInventoryCheckpoint
	must(json.Unmarshal(saved, &checkpoint))
	if len(checkpoint.Inventory.MachineUIDs) != 2 || len(checkpoint.Inventory.Publications[plan.MachineUID].Retirement) == 0 {
		t.Fatal("retirement reservation lost")
	}
	race := func(ctx context.Context, p tunnel.ReceiveTransitionPlan, v handover.View, uid string, routes tunnel.ReceiveJournalRoutes) error {
		if err := native(ctx, p, v, uid, routes); err != nil {
			return err
		}
		current, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		current.Annotations = map[string]string{"race": "after-native"}
		_, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, current, metav1.UpdateOptions{})
		return err
	}
	receipts, err = executeReceivePhase(ctx, api, store, scope, cfg, key, "container", race)
	if err == nil || len(receipts) != 0 || calls != 2 {
		t.Fatal("changed withdrawal produced survivor receipt", err)
	}
	t.Run("completed-maintenance", func(t *testing.T) {
		original, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
		must(err)
		defer func() {
			current, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
			must(err)
			current.Data = original.Data
			_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
			must(err)
		}()
		completeReceiveTestJournal(t, store)
		before := calls
		must(maintainCompletedReceivePhase(ctx, api, store, scope, cfg, key, "container", native))
		if calls != before+1 {
			t.Fatal("completed survivor maintenance did not execute")
		}
	})

	// Exclusion planning can remove authority before checkpoint authentication;
	// synthetic finalization must still fail before native execution is allowed.
	t.Run("released-exclusions-before-checkpoint", func(t *testing.T) {
		inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
		must(err)
		original := index.DeepCopy()
		defer func() {
			current, err := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
			must(err)
			current.Data = original.Data
			_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
			must(err)
		}()
		p := inventory.Publications[plan.MachineUID]
		p.RetirementCompletion, p.RetirementFinalization = json.RawMessage(`{}`), json.RawMessage(`{}`)
		inventory.Released = map[string]tunnel.ReceiveJournalPublication{plan.MachineUID: p}
		delete(inventory.Publications, plan.MachineUID)
		inventory.MachineUIDs = []string{scope.MachineUID}
		raw, err := json.Marshal(inventory)
		must(err)
		index.Data["inventory.json"] = string(raw)
		_, err = store.ConfigMaps.Update(ctx, index, metav1.UpdateOptions{})
		must(err)
		set, err := loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key)
		must(err)
		if set.Routes.Withdrawn[plan.MachineUID] != plan.RootPublicKey || len(set.Routes.Plans) != 1 {
			t.Fatal("released root lost its exclusion or retained active routes")
		}
		lock, err := lockReconciliation(cfg)
		must(err)
		defer lock.Close()
		if err := checkpointReceiveSetLocked(ctx, api, store, scope, cfg, key, lock.Name(), set); err == nil {
			t.Fatal("synthetic finalization authorized an executable checkpoint")
		}
	})

	t.Run("shared-retirement-admission", func(t *testing.T) {
		verifySharedSurvivorRetirements(t, api, store, scope, retiring, plan, w, key, cloudKey)
	})

}

func completeReceiveTestJournal(t *testing.T, store handover.Store) handover.Snapshot {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Load(ctx)
	must(err)
	for i := 0; ; i++ {
		if i > 10 {
			t.Fatal("retiring history did not complete")
		}
		view, err := snapshot.Transition().View()
		must(err)
		if view.Complete {
			break
		}
		at := time.Now()
		if earliest := view.Round.IssuedAt.Add(time.Second); at.Before(earliest) {
			at = earliest
		}
		if view.Round.Phase == handover.Drain && at.Before(view.DrainUntil) {
			at = view.DrainUntil
		}
		hash, err := view.Round.Digest()
		must(err)
		var proofs []handover.AuthenticatedReceipt
		for _, m := range view.Round.Members {
			proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: hash, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: at}})
		}
		next, err := snapshot.Transition().Advance(at, proofs)
		must(err)
		snapshot, err = store.Commit(ctx, snapshot, next)
		must(err)
	}
	return snapshot
}
