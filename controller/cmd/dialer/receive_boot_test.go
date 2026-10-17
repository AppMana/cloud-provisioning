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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Construct terminal fixture history, not production receipts. Native tests
// use this only after independently verifying the corresponding kernel state.
func writeReceiveBootFixture(t *testing.T, cfg config, root wgtypes.Key, plan tunnel.ReceiveTransitionPlan, legacy bool) (string, string, string) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(plan)
	must(err)
	spec, err := tunnel.ReceiveTransitionSpec(plan, "cluster", fmt.Sprintf("%x", sha256.Sum256(raw)))
	must(err)
	at := time.Now().Add(-10 * time.Second)
	tr, err := handover.NewTransition(spec, at, handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
	must(err)
	if legacy {
		tr, err = tr.Rollback(at)
		must(err)
	}
	for i := 1; i <= 5; i++ {
		view, err := tr.View()
		must(err)
		if view.Complete {
			break
		}
		digest, err := view.Round.Digest()
		must(err)
		var proofs []handover.AuthenticatedReceipt
		for _, m := range view.Round.Members {
			proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: digest, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: at.Add(time.Duration(i) * time.Second)}})
		}
		tr, err = tr.Advance(at.Add(time.Duration(i)*time.Second), proofs)
		must(err)
	}
	view, err := tr.View()
	must(err)
	if !view.Complete {
		t.Fatal("fixture did not complete")
	}
	encoded, err := tr.Bytes()
	must(err)
	store := handover.Store{Name: tunnel.ReceiveTransitionName(plan.MachineUID), ClusterUID: "cluster"}
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: plan.Namespace, Name: store.Name, UID: "journal", ResourceVersion: "1"}, Data: map[string]string{"plan.json": string(raw), "transition.json": string(encoded)}}
	journal, err := json.Marshal(map[string]any{"version": 1, "object": object})
	must(err)
	snapshot, err := store.RestoreCheckpoint(journal)
	must(err)
	journal, err = snapshot.Checkpoint()
	must(err)
	identity := tunnel.PeersFileDoc{PrivateKey: root.String(), LocalAddress: plan.CloudAddresses[0]}
	if len(plan.CloudAddresses) > 1 {
		identity.LocalAddress6 = plan.CloudAddresses[1]
	}
	must(writeDurableJSON(cfg.peersFile, identity))
	lock := filepath.Join(filepath.Dir(cfg.peersFile), cfg.iface+".reconcile.lock")
	inventory := receiveInventoryCheckpoint{Version: 1, InventoryUID: "inventory", NodeName: plan.CloudNodeName, NodeUID: plan.CloudNodeUID, BootID: "previous-boot", PublicKey: root.PublicKey().String(), Inventory: tunnel.ReceiveInventory{Version: 1, ClusterUID: "cluster", Namespace: plan.Namespace, MeshName: plan.MeshName, MeshUID: plan.MeshUID, MachineUIDs: []string{plan.MachineUID}, Publications: map[string]tunnel.ReceiveJournalPublication{plan.MachineUID: {JournalUID: "journal"}}}}
	inventoryPath := lock + ".inventory.json"
	must(writeDurableJSON(inventoryPath, inventory))
	inventoryRaw, err := os.ReadFile(inventoryPath)
	must(err)
	checkpointPath := lock + "." + store.Name + ".json"
	must(writeDurableJSON(checkpointPath, receiveJournalCheckpoint{Version: 1, Scope: receiveJournalScope{Namespace: plan.Namespace, MeshName: plan.MeshName, MeshUID: plan.MeshUID, MachineUID: plan.MachineUID}, ClusterUID: "cluster", NodeName: plan.CloudNodeName, NodeUID: plan.CloudNodeUID, BootID: inventory.BootID, PublicKey: root.PublicKey().String(), Journal: journal}))
	observationPath := receiveNativeObservationPath(checkpointPath, store.Name)
	// Prior agents read only this historical observation filename. New boot
	// recovery must ignore it and leave it intact for rollback compatibility.
	must(os.WriteFile(lock+".native-"+store.Name+".json", []byte(`{"journalSHA256":"old"}`), 0600))
	must(writeDurableJSON(observationPath, receiveNativeObservation{InventorySHA256: fmt.Sprintf("%x", sha256.Sum256(inventoryRaw)), LocalMachineUIDs: []string{plan.MachineUID}, Network: &receiveRecoveryNetwork{Interface: cfg.iface, RouteTable: 518, FWMark: 518, MTU: 1400, ListenPort: cfg.listenPort, KeepaliveSeconds: 15}, JournalSHA256: fmt.Sprintf("%x", sha256.Sum256(journal)), BootID: inventory.BootID, AgentPodUID: "pod", AgentContainerID: "container", ObservedAt: time.Now()}))
	return inventoryPath, checkpointPath, observationPath
}

func TestReceiveBootAdmission(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, fault := range []string{"valid", "unverified", "inventory", "local-set", "network", "identity", "allocation", "extra-journal", "same-boot", "changed-during-native", "cancelled", "busy", "unfinished"} {
			t.Run(fmt.Sprintf("legacy-%t/%s", legacy, fault), func(t *testing.T) {
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				root, err := wgtypes.GeneratePrivateKey()
				must(err)
				site, err := wgtypes.GeneratePrivateKey()
				must(err)
				plan := receiveRestoreTestPlan(t, root, site)
				cfg := config{iface: "bootroot", peersFile: filepath.Join(t.TempDir(), "peers.json"), routeTable: 517, fwmark: 517, mtu: 1420}
				inventoryPath, checkpointPath, observationPath := writeReceiveBootFixture(t, cfg, root, plan, legacy)
				raw, err := os.ReadFile(observationPath)
				must(err)
				var observation receiveNativeObservation
				must(json.Unmarshal(raw, &observation))
				var held *os.File
				if fault == "busy" {
					held, err = lockReconciliation(cfg)
					must(err)
					defer held.Close()
				}
				switch fault {
				case "unfinished":
					data, err := os.ReadFile(checkpointPath)
					must(err)
					var saved receiveJournalCheckpoint
					must(json.Unmarshal(data, &saved))
					var checkpoint struct {
						Version int               `json:"version"`
						Object  *corev1.ConfigMap `json:"object"`
					}
					must(json.Unmarshal(saved.Journal, &checkpoint))
					rawPlan := []byte(checkpoint.Object.Data["plan.json"])
					spec, err := tunnel.ReceiveTransitionSpec(plan, "cluster", fmt.Sprintf("%x", sha256.Sum256(rawPlan)))
					must(err)
					tr, err := handover.NewTransition(spec, time.Now(), handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
					must(err)
					encoded, err := tr.Bytes()
					must(err)
					checkpoint.Object.Data["transition.json"] = string(encoded)
					saved.Journal, err = json.Marshal(checkpoint)
					must(err)
					observation.JournalSHA256 = fmt.Sprintf("%x", sha256.Sum256(saved.Journal))
					must(writeDurableJSON(checkpointPath, saved))
					must(writeDurableJSON(observationPath, observation))
				case "unverified":
					must(os.Remove(observationPath))
				case "inventory":
					observation.InventorySHA256 = "other"
					must(writeDurableJSON(observationPath, observation))
				case "local-set":
					observation.LocalMachineUIDs = append(observation.LocalMachineUIDs, "another")
					must(writeDurableJSON(observationPath, observation))
				case "network":
					observation.Network = nil
					must(writeDurableJSON(observationPath, observation))
				case "identity":
					must(writeDurableJSON(cfg.peersFile, tunnel.PeersFileDoc{PrivateKey: site.String(), LocalAddress: plan.CloudAddresses[0]}))
				case "allocation":
					must(writeDurableJSON(cfg.peersFile, tunnel.PeersFileDoc{PrivateKey: root.String(), LocalAddress: "172.29.0.2/32"}))
				case "extra-journal":
					must(os.WriteFile(filepath.Join(filepath.Dir(cfg.peersFile), cfg.iface+".reconcile.lock.receive-transition-unaccounted.json"), []byte("{}"), 0600))
				case "same-boot":
					raw, err := os.ReadFile(inventoryPath)
					must(err)
					var inventory receiveInventoryCheckpoint
					must(json.Unmarshal(raw, &inventory))
					inventory.BootID, err = localReceiveBootID()
					must(err)
					must(writeDurableJSON(inventoryPath, inventory))
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if fault == "cancelled" {
					cancel()
				}
				calls := 0
				handled, err := admitReceiveBoot(ctx, cfg, func(state receiveBootState, check func() error) error {
					calls++
					if state.Key != root || state.NodeUID != plan.CloudNodeUID || state.Config.routeTable != 518 || state.Config.mtu != 1400 || !state.View.Complete {
						t.Fatal("boot recovery lost verified identity or network settings")
					}
					if lock, err := lockReconciliation(cfg); err == nil {
						lock.Close()
						t.Fatal("boot recovery escaped host lock")
					}
					if fault == "changed-during-native" {
						must(os.WriteFile(observationPath, []byte("{}"), 0600))
					}
					return check()
				})
				if !handled {
					t.Fatal("reserved boot fell through to legacy")
				}
				if fault == "same-boot" {
					if err != nil || calls != 0 {
						t.Fatal("host competed with live same-boot owner", err)
					}
					return
				}
				if fault == "valid" {
					must(err)
					if calls != 1 {
						t.Fatal("valid boot not restored")
					}
					return
				}
				if err == nil {
					t.Fatal("unsafe boot admitted")
				}
				want := 0
				if fault == "changed-during-native" {
					want = 1
				}
				if calls != want {
					t.Fatalf("native calls %d want %d", calls, want)
				}
			})
		}
	}
}

func TestReceiveBootMissingInventoryKeepsLegacyFence(t *testing.T) {
	root, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	site, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{iface: "bootroot", peersFile: filepath.Join(t.TempDir(), "peers.json")}
	inventory, _, _ := writeReceiveBootFixture(t, cfg, root, receiveRestoreTestPlan(t, root, site), false)
	if err := os.Remove(inventory); err != nil {
		t.Fatal(err)
	}
	handled, err := admitReceiveBoot(context.Background(), cfg, func(receiveBootState, func() error) error {
		t.Fatal("missing inventory reached native recovery")
		return nil
	})
	if err != nil || handled {
		t.Fatal("unexpected boot inventory result", err)
	}
	if lock, err := lockLegacyReconciliation(cfg); err == nil {
		lock.Close()
		t.Fatal("incomplete checkpoint set allowed legacy replay")
	}
}

// Consume an observation produced by the real execution boundary with the
// fixture API. Only the saved boot IDs change to model a subsequent boot;
// membership and network bindings must come from the production writer.
func verifyRecordedReceiveBoot(t *testing.T, cfg config, plan tunnel.ReceiveTransitionPlan, key wgtypes.Key) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	lock := filepath.Join(filepath.Dir(cfg.peersFile), cfg.iface+".reconcile.lock")
	journalName := tunnel.ReceiveTransitionName(plan.MachineUID)
	checkpointPath := lock + "." + journalName + ".json"
	inventoryPath := lock + ".inventory.json"
	observationPath := receiveNativeObservationPath(checkpointPath, journalName)
	originals := map[string][]byte{}
	for _, path := range []string{checkpointPath, inventoryPath, observationPath} {
		raw, err := os.ReadFile(path)
		must(err)
		originals[path] = raw
	}
	defer func() {
		for path, raw := range originals {
			must(os.WriteFile(path, raw, 0600))
		}
	}()
	var saved receiveJournalCheckpoint
	must(json.Unmarshal(originals[checkpointPath], &saved))
	var inventory receiveInventoryCheckpoint
	must(json.Unmarshal(originals[inventoryPath], &inventory))
	var observation receiveNativeObservation
	must(json.Unmarshal(originals[observationPath], &observation))
	saved.BootID = "recorded-previous-boot"
	inventory.BootID = saved.BootID
	observation.BootID = saved.BootID
	must(writeDurableJSON(checkpointPath, saved))
	must(writeDurableJSON(inventoryPath, inventory))
	raw, err := os.ReadFile(inventoryPath)
	must(err)
	observation.InventorySHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	must(writeDurableJSON(observationPath, observation))
	host := cfg
	host.receiptURL = ""
	host.nodeName = ""
	host.routeTable = 517
	host.fwmark = 517
	host.mtu = 1420
	calls := 0
	handled, err := admitReceiveBoot(context.Background(), host, func(state receiveBootState, check func() error) error {
		calls++
		if state.Key != key || state.NodeUID != plan.CloudNodeUID || state.Config.routeTable != cfg.routeTable || state.Config.fwmark != cfg.fwmark || state.Config.mtu != cfg.mtu {
			t.Fatal("production observation lost its boot bindings")
		}
		return check()
	})
	must(err)
	if !handled || calls != 1 {
		t.Fatal("production observation did not admit verified boot recovery")
	}
}
