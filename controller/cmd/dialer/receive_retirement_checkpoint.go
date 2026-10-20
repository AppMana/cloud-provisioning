package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"k8s.io/client-go/kubernetes"
)

func sameReceiveRetirement(a, b admittedReceiveRetirement) bool {
	if !sameReleasedReceiveJournals(a.ReleasedJournals, b.ReleasedJournals) || !sameReceiveRevision(a.Metadata, b.Metadata) || !sameReceiveRevision(a.MeshMetadata, b.MeshMetadata) || a.NodeUID != b.NodeUID || a.BootID != b.BootID || len(a.Journals) != len(b.Journals) {
		return false
	}
	for uid, snapshot := range a.Journals {
		other, found := b.Journals[uid]
		if !found || other.ContinuesExecution(snapshot) != nil {
			return false
		}
	}
	return true
}

// Caller holds the existing host lock. Validate every local history before
// writing anything, then durably record the inventory before journal files.
// That order fences old serving-state recovery even if a later write fails.
// This is retirement intent, never evidence that native cleanup completed.
func checkpointReceiveRetirementLocked(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, lockPath string, admitted admittedReceiveRetirement) error {
	return checkpointReceiveRetirementWithReleasedLocked(ctx, api, store, scope, cfg, key, lockPath, admitted, nil)
}

// A set checkpoint may pass its already authenticated release histories. The
// exact next checkpoint must match, and live rechecks still run at each write.
// The admission is local to this locked operation, never a reconciliation cache.
func checkpointReceiveRetirementWithReleasedLocked(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, lockPath string, admitted admittedReceiveRetirement, shared *receiveReleasedCheckpointAdmission) error {
	if admitted.Mesh == nil || scope.Namespace != admitted.Mesh.Namespace || scope.MeshName != admitted.Mesh.Name || scope.MeshUID != string(admitted.Mesh.UID) || scope.MachineUID != admitted.Withdrawal.MachineUID || store.Name != tunnel.ReceiveTransitionName(scope.MachineUID) {
		return fmt.Errorf("retirement checkpoint differs from admitted scope")
	}
	target, found := admitted.Journals[scope.MachineUID]
	if !found || (store.ExpectedUID != "" && store.ExpectedUID != string(target.Metadata().UID)) {
		return fmt.Errorf("retirement checkpoint journal incarnation changed")
	}
	if err := recheckReceiveRetirement(ctx, api, store, admitted, cfg.nodeName, key); err != nil {
		return err
	}
	next := receiveInventoryCheckpoint{Version: 1, InventoryUID: string(admitted.Metadata.UID), Inventory: admitted.Inventory, NodeName: cfg.nodeName, NodeUID: admitted.NodeUID, BootID: admitted.BootID, PublicKey: key.PublicKey().String()}
	path := lockPath + ".inventory.json"
	var released receiveReleasedCheckpointAdmission
	if shared == nil {
		var err error
		released, err = admitReleasedReceiveCheckpoints(ctx, api, store, scope, cfg, key, lockPath, next, admitted.Metadata)
		if err != nil {
			return err
		}
	} else {
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if shared.nextSHA == "" || shared.nextSHA != tunnel.HashPeerList(raw) || len(shared.records) != len(next.Inventory.Released) {
			return fmt.Errorf("shared released checkpoint admission differs from write")
		}
		released = *shared
	}
	if err := validateReceiveInventoryCheckpointWithRelease(path, next, released); err != nil {
		return err
	}
	checkpoints := map[string]receiveJournalCheckpoint{}
	for _, plan := range admitted.Plans {
		memberScope := scope
		memberScope.MachineUID = plan.MachineUID
		uid, err := receiveJournalParticipant(plan, memberScope, cfg.nodeName, key)
		if err != nil {
			// Ordinary consumers retain the inventory fence but do not acquire
			// receiver-phase membership merely by acknowledging withdrawal.
			continue
		}
		if uid != admitted.NodeUID {
			return fmt.Errorf("retirement local journal identity differs")
		}
		memberStore := store
		memberStore.Name = tunnel.ReceiveTransitionName(plan.MachineUID)
		if plan.MachineUID != scope.MachineUID {
			memberStore.ExpectedUID = ""
		}
		snapshot := admitted.Journals[plan.MachineUID]
		journalPath := lockPath + "." + memberStore.Name + ".json"
		previous, err := readReceiveJournalCheckpointForAdmission(journalPath, memberStore, memberScope, cfg.nodeName, key, true)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if err := memberStore.CheckHistory(ctx, snapshot, previous.Snapshot); err != nil {
				return err
			}
		}
		raw, err := snapshot.Checkpoint()
		if err != nil {
			return err
		}
		checkpoints[journalPath] = receiveJournalCheckpoint{Version: 1, Scope: memberScope, ClusterUID: store.ClusterUID, NodeName: cfg.nodeName, NodeUID: uid, BootID: admitted.BootID, PublicKey: key.PublicKey().String(), Journal: raw}
	}
	entries, err := os.ReadDir(filepath.Dir(lockPath))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(lockPath)+".receive-transition-") {
			if _, found := checkpoints[filepath.Join(filepath.Dir(lockPath), entry.Name())]; !found && !released.localFiles[entry.Name()] {
				return fmt.Errorf("retirement omitted a checkpointed local journal")
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := released.recheck(ctx, api, store); err != nil {
		return err
	}
	if err := writeDurableJSON(path, next); err != nil {
		return err
	}
	for name, checkpoint := range checkpoints {
		if err := writeDurableJSON(name, checkpoint); err != nil {
			return err
		}
	}
	return nil
}
