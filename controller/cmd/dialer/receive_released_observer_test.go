package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Real API and private durable files, with synthetic finalized/released evidence.
// Exercises unpublished observer checkpoint advancement while another receiver
// remains active. Native callbacks must never run. No actual reboot, kernel
// absence, stale-worker replay or address reuse is qualified by this fixture.
func verifyReleasedObserverCheckpoint(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, before []byte) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg.secretNamespace, cfg.secretName, cfg.receiptMeshName = scope.Namespace, scope.MeshName, scope.MeshName
	must(os.WriteFile(cfg.privateKeyFile, []byte(key.String()), 0600))
	lockPath := filepath.Join(filepath.Dir(cfg.privateKeyFile), cfg.iface+".reconcile.lock")
	saved, err := os.ReadFile(lockPath + ".inventory.json")
	must(err)
	mesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
	must(err)
	originalMesh := mesh.DeepCopy()
	_, originalIndex, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	must(err)
	foreignRoot, err := wgtypes.GeneratePrivateKey()
	must(err)
	foreignSite, err := wgtypes.GeneratePrivateKey()
	must(err)
	plan := receiveRestoreTestPlan(t, foreignRoot, foreignSite)
	plan.Namespace, plan.MeshName, plan.MeshUID = scope.Namespace, scope.MeshName, scope.MeshUID
	plan.MachineName, plan.MachineUID = "observer-foreign", "observer-foreign-uid"
	plan.AdoptionName = tunnel.AdoptionSecretName(plan.MachineName)
	plan.CloudNodeName, plan.CloudNodeUID = "observer-foreign-cloud", "observer-foreign-cloud-uid"
	pub := foreignSite.PublicKey().String()
	prep, err := tunnel.PlanReceiveHandover(scope.MeshUID, plan.Desired, plan.Desired, nil)
	must(err)
	binding := plan.Bindings[pub]
	binding.Interface = prep.Receivers[0].Interface
	binding.PublicKey = receiverKey(foreignRoot, scope.MeshUID, pub).PublicKey().String()
	plan.Bindings[pub] = binding
	member := plan.Sites[pub]
	member.NodeName, member.NodeUID = "observer-foreign-site", "observer-foreign-site-uid"
	member.Client, err = tunnel.PlanSiteReceiveClient(scope.MeshUID, pub, plan.RootPublicKey, binding, plan.SiteDesired, plan.SiteDesired)
	must(err)
	plan.Sites[pub] = member
	foreign := store
	foreign.Name = tunnel.ReceiveTransitionName(plan.MachineUID)
	foreign.ExpectedUID = ""
	defer func() {
		must(os.WriteFile(lockPath+".inventory.json", saved, 0600))
		m, e := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
		must(e)
		m.Data = originalMesh.Data
		_, e = api.CoreV1().Secrets(scope.Namespace).Update(ctx, m, metav1.UpdateOptions{})
		must(e)
		i, e := store.ConfigMaps.Get(ctx, originalIndex.Name, metav1.GetOptions{})
		must(e)
		i.Data = originalIndex.Data
		_, e = store.ConfigMaps.Update(ctx, i, metav1.UpdateOptions{})
		must(e)
		must(store.ConfigMaps.Delete(ctx, foreign.Name, metav1.DeleteOptions{}))
	}()
	raw, err := json.Marshal(plan)
	must(err)
	spec, err := tunnel.ReceiveTransitionSpec(plan, store.ClusterUID, fmt.Sprintf("%x", sha256.Sum256(raw)))
	must(err)
	tr, err := handover.NewTransition(spec, time.Now(), handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
	must(err)
	must(tunnel.ReserveReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, plan.MachineUID, store.OwnerReferences))
	snapshot, err := foreign.CreateWithPlan(ctx, tr, raw)
	must(err)
	inv, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	must(err)
	if inv.Publications == nil {
		inv.Publications = map[string]tunnel.ReceiveJournalPublication{}
	}
	inv.Publications[plan.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(snapshot.Metadata().UID)}
	raw, err = json.Marshal(inv)
	must(err)
	index.Data["inventory.json"] = string(raw)
	index, err = store.ConfigMaps.Update(ctx, index, metav1.UpdateOptions{})
	must(err)
	currentData := index.DeepCopy().Data
	for _, fault := range []string{"none", "previous-boot", "missing-fence", "changed-fence", "unknown-journal", "neighbor-owner", "wrong-agent", "cancelled", "inventory-race", "mesh-race", "missing-active-journal"} {
		t.Run("released-observer/"+fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			m, e := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
			must(e)
			delete(m.Data, tunnel.NodePublicKeyPrefix+cfg.nodeName)
			delete(m.Data, tunnel.NodeTunnelAddressPrefix+cfg.nodeName)
			delete(m.Data, tunnel.NodeTunnelAddress6Prefix+cfg.nodeName)
			m, e = api.CoreV1().Secrets(scope.Namespace).Update(ctx, m, metav1.UpdateOptions{})
			must(e)
			i, e := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
			must(e)
			i.Data = currentData
			_, e = store.ConfigMaps.Update(ctx, i, metav1.UpdateOptions{})
			must(e)
			must(os.WriteFile(lockPath+".inventory.json", before, 0600))
			runCfg := cfg
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			switch fault {
			case "previous-boot", "changed-fence":
				var old receiveInventoryCheckpoint
				must(json.Unmarshal(before, &old))
				if fault == "previous-boot" {
					old.BootID = "previous-boot"
				} else {
					p := old.Inventory.Publications[scope.MachineUID]
					p.Retirement = json.RawMessage(`{"changed":true}`)
					old.Inventory.Publications[scope.MachineUID] = p
				}
				must(writeDurableJSON(lockPath+".inventory.json", old))
			case "missing-fence":
				must(os.Remove(lockPath + ".inventory.json"))
			case "unknown-journal":
				name := lockPath + ".receive-transition-untracked.json"
				must(os.WriteFile(name, []byte("{}"), 0600))
				defer os.Remove(name)
			case "neighbor-owner":
				var old receiveInventoryCheckpoint
				must(json.Unmarshal(before, &old))
				name := lockPath + ".neighbors.json"
				must(writeDurableJSON(name, receiveNeighborCheckpoint{Version: 1, InventoryUID: old.InventoryUID, MeshUID: scope.MeshUID, MeshVersion: m.ResourceVersion, NodeUID: old.NodeUID, BootID: old.BootID, PublicKey: old.PublicKey, Peers: plan.SiteDesired.Peers}))
				defer os.Remove(name)
			case "wrong-agent":
				runCfg.receiptPodUID = "replacement"
			case "cancelled":
				cancel()
			case "missing-active-journal":
				changed := inv
				changed.Publications = map[string]tunnel.ReceiveJournalPublication{}
				for uid, p := range inv.Publications {
					changed.Publications[uid] = p
				}
				p := changed.Publications[plan.MachineUID]
				p.JournalUID = "replacement"
				changed.Publications[plan.MachineUID] = p
				data, e := json.Marshal(changed)
				must(e)
				i, e = store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
				must(e)
				i.Data = map[string]string{"inventory.json": string(data)}
				_, e = store.ConfigMaps.Update(ctx, i, metav1.UpdateOptions{})
				must(e)
			}
			prior, _ := os.ReadFile(lockPath + ".inventory.json")
			calls := 0
			native := func(context.Context, wgtypes.Key, []tunnel.PeerSpec, []string, []tunnel.PeerSpec, []string, func() error) error {
				calls++
				return fmt.Errorf("observer reached native routing")
			}
			if fault == "inventory-race" || fault == "mesh-race" {
				lock, lockErr := lockReconciliation(runCfg)
				must(lockErr)
				defer lock.Close()
				changed := false
				counted := &changingJournalReader{ConfigMapInterface: store.ConfigMaps, change: func() error {
					changed = true
					if fault == "inventory-race" {
						i, e := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
						if e != nil {
							return e
						}
						i.Annotations = map[string]string{"observer-race": fault}
						_, e = store.ConfigMaps.Update(ctx, i, metav1.UpdateOptions{})
						return e
					}
					obj, e := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
					if e != nil {
						return e
					}
					obj.Annotations = map[string]string{"observer-race": fault}
					_, e = api.CoreV1().Secrets(scope.Namespace).Update(ctx, obj, metav1.UpdateOptions{})
					return e
				}}
				reader := store
				reader.ConfigMaps = counted
				observerScope := scope
				observerScope.MachineUID = ""
				e = checkpointReleasedReceiveObserver(runCtx, api, reader, observerScope, runCfg, key, lock.Name(), m)
				if !changed {
					t.Fatal("source race was not exercised")
				}
			} else {
				e = reconcileReceiveNeighbors(runCtx, api, runCfg, native, nil)
			}
			if calls != 0 {
				t.Fatal("observer invoked routing callback")
			}
			after, _ := os.ReadFile(lockPath + ".inventory.json")
			if fault != "none" && fault != "previous-boot" {
				if e == nil {
					t.Fatal("invalid observer checkpoint accepted", fault)
				}
				if string(prior) != string(after) {
					t.Fatal("failed observer changed saved fence")
				}
				return
			}
			must(e)
			var checkpoint receiveInventoryCheckpoint
			must(json.Unmarshal(after, &checkpoint))
			if len(checkpoint.Inventory.Released) != 1 || len(checkpoint.Inventory.MachineUIDs) != 1 || checkpoint.Inventory.MachineUIDs[0] != plan.MachineUID {
				t.Fatal("observer lost active or released inventory")
			}
			if checkpoint.BootID == "previous-boot" {
				t.Fatal("observer did not admit current boot")
			}
		})
	}
}
