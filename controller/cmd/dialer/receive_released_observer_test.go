package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Real API and private durable files, with synthetic finalized/released evidence.
// Exercises unpublished observer checkpoint advancement while another receiver
// remains active. Ordinary-neighbor native callbacks must never run. Separate
// simulated observer callbacks exercise admission races without kernel writes.
// No actual reboot, kernel absence, stale-worker replay or address reuse is
// qualified by this fixture.
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
	plan.Desired.Peers[0].RouteHosts = []string{"10.90.0.2", "fd90::2"}
	plan.Base, err = json.Marshal(plan.Desired)
	must(err)
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
	verifyUnreservedCloudDispatch(t, api, store, scope, foreignRoot, key)
	for _, fault := range []string{"none", "previous-boot", "missing-fence", "changed-fence", "unknown-journal", "neighbor-owner", "wrong-agent", "cancelled", "inventory-race", "mesh-race", "missing-active-journal", "callback-success", "callback-neighbor-dispatch", "callback-mesh-race", "callback-node-race", "callback-inventory-race", "callback-agent-race", "callback-cancelled", "callback-post-use-race"} {
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
			if strings.HasPrefix(fault, "callback-") {
				if m.Data == nil {
					m.Data = map[string][]byte{}
				}
				m.Data[tunnel.PeerPublicKeyPrefix+plan.MachineName] = []byte(plan.RootPublicKey)
				m.Data[tunnel.NodePublicKeyPrefix+member.NodeName] = []byte(pub)
				m.Data[tunnel.NodeAddressesPrefix+member.NodeName] = []byte("10.90.0.2,fd90::2")
				m.Data[tunnel.SiteAddressesPrefix+cfg.nodeName] = []byte("10.90.0.4,fd90::4")
			}
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
			} else if strings.HasPrefix(fault, "callback-") {
				// Simulated native boundary only. The real API must reject changed
				// authority before a callback could write, and again on return.
				lock, lockErr := lockReconciliation(runCfg)
				must(lockErr)
				defer lock.Close()
				observerScope := scope
				observerScope.MachineUID = ""
				uses, writes := 0, 0
				use := func(useCtx context.Context, admitted receiveObserverAdmission, check func() error) error {
					uses++
					if len(admitted.Inventory.Released) != 1 || len(admitted.Journals) != 1 || admitted.Journals[plan.MachineUID].Metadata().UID != snapshot.Metadata().UID || admitted.Node.Name != cfg.nodeName || admitted.Mesh.UID != mesh.UID {
						t.Fatal("callback lost admitted observer scope")
					}
					if deadline, ok := useCtx.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
						t.Fatal("observer callback lacks bounded authority")
					}
					must(check())
					projected, err := admitReceiveFollowerJournalPath(admitted.Mesh.Data, admitted.Node.Name, string(admitted.Node.UID), admitted.Journals[plan.MachineUID], store.ClusterUID, time.Now(), func(owner tunnel.ReceiveTransitionSite, receipt handover.Receipt) error {
						return handover.VerifyObservationIncarnation(useCtx, api, scope.Namespace, owner.NodeName, receipt)
					})
					must(err)
					if projected.Transit == nil || projected.Transit.Via != "10.90.0.2" || projected.Transit.ViaOtherFamily != "fd90::2" {
						t.Fatal("observer admission lost the worker's committed relay")
					}
					switch fault {
					case "callback-mesh-race", "callback-post-use-race":
						obj, err := api.CoreV1().Secrets(scope.Namespace).Get(useCtx, scope.MeshName, metav1.GetOptions{})
						must(err)
						obj.Annotations = map[string]string{"test/observer-callback": fault}
						_, err = api.CoreV1().Secrets(scope.Namespace).Update(useCtx, obj, metav1.UpdateOptions{})
						must(err)
					case "callback-inventory-race":
						obj, err := store.ConfigMaps.Get(useCtx, index.Name, metav1.GetOptions{})
						must(err)
						obj.Annotations = map[string]string{"test/observer-callback": fault}
						_, err = store.ConfigMaps.Update(useCtx, obj, metav1.UpdateOptions{})
						must(err)
					case "callback-node-race":
						obj, err := api.CoreV1().Nodes().Get(useCtx, cfg.nodeName, metav1.GetOptions{})
						must(err)
						savedStatus := obj.DeepCopy().Status
						defer func() {
							current, err := api.CoreV1().Nodes().Get(ctx, cfg.nodeName, metav1.GetOptions{})
							must(err)
							current.Status = savedStatus
							_, err = api.CoreV1().Nodes().UpdateStatus(ctx, current, metav1.UpdateOptions{})
							must(err)
						}()
						obj.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.199"}}
						_, err = api.CoreV1().Nodes().UpdateStatus(useCtx, obj, metav1.UpdateOptions{})
						must(err)
					case "callback-agent-race":
						obj, err := api.CoreV1().Pods(scope.Namespace).Get(useCtx, cfg.receiptPodName, metav1.GetOptions{})
						must(err)
						savedStatus := obj.DeepCopy().Status
						defer func() {
							current, err := api.CoreV1().Pods(scope.Namespace).Get(ctx, cfg.receiptPodName, metav1.GetOptions{})
							must(err)
							current.Status = savedStatus
							_, err = api.CoreV1().Pods(scope.Namespace).UpdateStatus(ctx, current, metav1.UpdateOptions{})
							must(err)
						}()
						for j := range obj.Status.ContainerStatuses {
							obj.Status.ContainerStatuses[j].ContainerID = "containerd://replacement"
						}
						_, err = api.CoreV1().Pods(scope.Namespace).UpdateStatus(useCtx, obj, metav1.UpdateOptions{})
						must(err)
					case "callback-cancelled":
						cancel()
					}
					if fault == "callback-post-use-race" {
						return nil // Outer boundary must detect the changed mesh.
					}
					if err := check(); err != nil {
						return err
					}
					writes++
					return nil
				}
				if fault == "callback-neighbor-dispatch" {
					must(lock.Close())
					e = reconcileReceiveNeighbors(runCtx, api, runCfg, native, nil, func(useCtx context.Context, gotKey wgtypes.Key, gotPath string, admitted receiveObserverAdmission, check func() error) error {
						if gotKey != key || gotPath != lockPath {
							t.Fatal("follower dispatch lost host identity")
						}
						return use(useCtx, admitted, check)
					})
				} else {
					e = withReleasedReceiveObserver(runCtx, api, store, observerScope, runCfg, key, lock.Name(), m, use)
				}
				success := fault == "callback-success" || fault == "callback-neighbor-dispatch"
				if uses != 1 || (success && writes != 1) || (!success && writes != 0) {
					t.Fatalf("callback escaped admission boundary: uses=%d writes=%d", uses, writes)
				}
			} else {
				e = reconcileReceiveNeighbors(runCtx, api, runCfg, native, nil)
			}
			if calls != 0 {
				t.Fatal("observer invoked routing callback")
			}
			after, _ := os.ReadFile(lockPath + ".inventory.json")
			if fault != "none" && fault != "previous-boot" && fault != "callback-success" && fault != "callback-neighbor-dispatch" {
				if e == nil {
					t.Fatal("invalid observer checkpoint accepted", fault)
				}
				if !strings.HasPrefix(fault, "callback-") && string(prior) != string(after) {
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
