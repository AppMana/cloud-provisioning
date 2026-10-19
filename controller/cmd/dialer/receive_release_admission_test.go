package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Real API and durable files with synthetic legacy finalization, release
// authority and native cleanup. This qualifies checkpoint admission only, not
// actual reboot, current native absence, release-controller policy or reuse.
func verifyReleasedCheckpointAdmission(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, key wgtypes.Key, pod *corev1.Pod) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	seedReceiveFinalization(t, api, store, scope, false)
	cfg := config{iface: "releasecheck", nodeName: "site", privateKeyFile: filepath.Join(t.TempDir(), "key"), receiptPodName: pod.Name, receiptPodUID: string(pod.UID)}
	receipts, err := executeReceiveRetirement(ctx, api, store, scope, cfg, key, "containerd://agent", func(context.Context, string, admittedReceiveRetirement) error { return nil })
	must(err)
	if len(receipts) != 0 {
		t.Fatal("finalized fixture renewed receipts")
	}
	admitted, err := loadReceiveRetirement(ctx, api, store, scope, cfg.nodeName, key)
	must(err)
	must(tunnel.ReleaseReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, admitted.Metadata, admitted.Mesh, admitted.Journals[scope.MachineUID], admitted.Inventory.Publications[scope.MachineUID], func() error { return nil }))
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	must(err)
	lock, err := lockReconciliation(cfg)
	must(err)
	defer lock.Close()
	path := lock.Name() + ".inventory.json"
	journalPath := lock.Name() + "." + store.Name + ".json"
	before, err := os.ReadFile(path)
	must(err)
	journalBefore, err := os.ReadFile(journalPath)
	must(err)
	var next receiveInventoryCheckpoint
	must(json.Unmarshal(before, &next))
	next.Inventory = inventory
	if validateReceiveInventoryCheckpoint(path, next) == nil {
		t.Fatal("unadmitted shrinkage accepted")
	}
	for _, fault := range []string{"none", "withdrawal-only-fence", "previous-boot", "missing-fence", "changed-intent", "changed-finalization", "missing-journal", "public-fence", "wrong-next-boot"} {
		t.Run(fault, func(t *testing.T) {
			defer func() {
				must(os.WriteFile(path, before, 0600))
				must(os.Chmod(path, 0600))
				must(os.WriteFile(journalPath, journalBefore, 0600))
			}()
			var saved receiveInventoryCheckpoint
			must(json.Unmarshal(before, &saved))
			candidate := next
			p := saved.Inventory.Publications[scope.MachineUID]
			switch fault {
			case "withdrawal-only-fence":
				p.RetirementCompletion = nil
				p.RetirementFinalization = nil
			case "previous-boot":
				saved.BootID = "prior-boot"
			case "changed-intent":
				p.Retirement = json.RawMessage(`{"changed":true}`)
			case "changed-finalization":
				p.RetirementFinalization = json.RawMessage(`{"changed":true}`)
			case "wrong-next-boot":
				candidate.BootID = "foreign-boot"
			}
			saved.Inventory.Publications[scope.MachineUID] = p
			must(writeDurableJSON(path, saved))
			switch fault {
			case "missing-fence":
				must(os.Remove(path))
			case "missing-journal":
				must(os.Remove(journalPath))
			case "public-fence":
				must(os.Chmod(path, 0644))
			}
			proof, err := admitReleasedReceiveCheckpoints(ctx, api, store, scope, cfg, key, lock.Name(), candidate, index.ObjectMeta)
			valid := fault == "none" || fault == "withdrawal-only-fence" || fault == "previous-boot"
			if !valid {
				if err == nil {
					t.Fatal("invalid local release fence admitted", fault)
				}
				return
			}
			must(err)
			must(validateReceiveInventoryCheckpointWithRelease(path, candidate, proof))
			must(proof.recheck(ctx, api, store))
			if fault == "none" {
				node, e := api.CoreV1().Nodes().Get(ctx, cfg.nodeName, metav1.GetOptions{})
				must(e)
				boot := node.Status.NodeInfo.BootID
				node.Status.NodeInfo.BootID = "changed-at-release-write"
				node, e = api.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
				must(e)
				rejected := proof.recheck(ctx, api, store)
				node.Status.NodeInfo.BootID = boot
				_, e = api.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
				must(e)
				if rejected == nil {
					t.Fatal("release write boundary accepted changed boot")
				}
			}
			if !proof.localFiles[filepath.Base(journalPath)] {
				t.Fatal("retired local journal omitted")
			}
			changed := candidate
			changed.NodeUID = "replacement"
			if validateReceiveInventoryCheckpointWithRelease(path, changed, proof) == nil {
				t.Fatal("release admission transferred to another checkpoint")
			}
		})
	}
	proof, err := admitReleasedReceiveCheckpoints(ctx, api, store, scope, cfg, key, lock.Name(), next, index.ObjectMeta)
	must(err)
	must(validateReceiveInventoryCheckpointWithRelease(path, next, proof))
	must(proof.recheck(ctx, api, store))
	must(writeDurableJSON(path, next))
	must(validateReceiveInventoryCheckpoint(path, next))
	after, err := os.ReadFile(journalPath)
	must(err)
	if string(after) != string(journalBefore) {
		t.Fatal("release admission rewrote retained local journal")
	}
	must(lock.Close())
	verifyReleasedObserverCheckpoint(t, api, store, scope, cfg, key, before)
	verifyReleasedNeighborRecovery(t, api, store, scope, cfg, key)
}
