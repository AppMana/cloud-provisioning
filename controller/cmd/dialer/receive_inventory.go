package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type admittedReceiveSet struct {
	Inventory        tunnel.ReceiveInventory
	Metadata         metav1.ObjectMeta
	Journals         map[string]handover.Snapshot
	ReleasedJournals map[string]handover.Snapshot
	Local            map[string]admittedReceiveJournal
	Routes           tunnel.ReceiveJournalRoutes
	Retirements      map[string]admittedReceiveRetirement
}

// Load every controller-reserved journal, not only the target Machine. A missing
// entry or changed execution authority invalidates the observation. Local members
// also undergo live key, Node, boot and adoption admission. This is read-only;
// neither index membership nor a route plan proves native phase completion.
func loadReceiveSet(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, nodeName string, key wgtypes.Key) (admittedReceiveSet, error) {
	fail := func(err error) (admittedReceiveSet, error) { return admittedReceiveSet{}, err }
	if api == nil || nodeName == "" || key == (wgtypes.Key{}) || scope.MachineUID == "" || store.Name != tunnel.ReceiveTransitionName(scope.MachineUID) {
		return fail(fmt.Errorf("receiver set requires admitted local and target identities"))
	}
	inventory, object, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	if err != nil {
		return fail(err)
	}
	for _, publication := range inventory.Publications {
		if len(publication.Retirement) != 0 {
			if len(inventory.Publications[scope.MachineUID].Retirement) != 0 {
				return fail(fmt.Errorf("receiver retirement intent requires withdrawal reconciliation"))
			}
			return loadSurvivingReceiveSet(ctx, api, store, scope, nodeName, key, inventory, object.ObjectMeta)
		}
	}
	result := admittedReceiveSet{Inventory: inventory, Metadata: *object.ObjectMeta.DeepCopy(), Journals: map[string]handover.Snapshot{}, Local: map[string]admittedReceiveJournal{}}
	result.ReleasedJournals, err = loadReleasedReceiveJournals(ctx, store, inventory)
	if err != nil {
		return fail(err)
	}
	var snapshots []handover.Snapshot
	for _, uid := range inventory.MachineUIDs {
		currentStore := store
		currentStore.Name = tunnel.ReceiveTransitionName(uid)
		if uid != scope.MachineUID {
			currentStore.ExpectedUID = ""
		}
		snapshot, err := currentStore.Load(ctx)
		if err != nil {
			return fail(fmt.Errorf("loading reserved receiver journal %s: %w", currentStore.Name, err))
		}
		if err := inventory.CheckJournal(uid, string(snapshot.Metadata().UID)); err != nil {
			return fail(err)
		}
		if err := tunnel.ValidateReceiveTransportHistory(ctx, currentStore, snapshot); err != nil {
			return fail(err)
		}
		plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
		if err != nil {
			return fail(err)
		}
		if plan.MachineUID != uid || plan.Namespace != scope.Namespace || plan.MeshName != scope.MeshName || plan.MeshUID != scope.MeshUID {
			return fail(fmt.Errorf("reserved journal differs from inventory scope"))
		}
		local := plan.CloudNodeName == nodeName || plan.RootPublicKey == key.PublicKey().String()
		for public, site := range plan.Sites {
			local = local || site.NodeName == nodeName || public == key.PublicKey().String()
		}
		if local {
			memberScope := scope
			memberScope.MachineUID = uid
			admitted, err := loadReceiveJournal(ctx, api, currentStore, memberScope, nodeName, key)
			if err != nil {
				return fail(err)
			}
			if admitted.Snapshot.ContinuesExecution(snapshot) != nil {
				return fail(fmt.Errorf("reserved journal changed during local admission"))
			}
			result.Local[uid] = admitted
			snapshot = admitted.Snapshot
		}
		if !local {
			// Nonlocal journals can own shared destinations. Their ancestry cannot be
			// skipped merely because this process will not issue their phase receipts.
			if _, found := snapshot.Predecessor(); found {
				if _, err := currentStore.ArchiveNames(ctx, snapshot); err != nil {
					return fail(err)
				}
			}
		}
		result.Journals[uid] = snapshot
		snapshots = append(snapshots, snapshot)
	}
	if _, found := result.Local[scope.MachineUID]; !found {
		return fail(fmt.Errorf("target Machine is not a local reserved journal"))
	}
	result.Routes, err = tunnel.PlanReceiveJournalRoutes(snapshots, store.ClusterUID)
	if err != nil {
		return fail(err)
	}
	// A later journal's admission must not conceal changes to an earlier one.
	// Reuse the bounded named-read guard: unchanged objects need no second
	// history replay, and every read completes before admitting the set.
	if err := store.RecheckExecutions(ctx, snapshots); err != nil {
		return fail(fmt.Errorf("receiver journal set changed during admission: %w", err))
	}
	_, current, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	if err != nil {
		return fail(err)
	}
	if !sameReceiveRevision(result.Metadata, current.ObjectMeta) {
		return fail(fmt.Errorf("receiver inventory changed during admission"))
	}
	if err := appendReleasedReceiveExclusions(inventory, &result.Routes); err != nil {
		return fail(err)
	}
	if err := recheckReleasedReceiveJournals(ctx, store, result.ReleasedJournals); err != nil {
		return fail(err)
	}
	return result, nil
}

func sameReceiveRevision(a, b metav1.ObjectMeta) bool {
	return a.UID != "" && a.UID == b.UID && a.ResourceVersion != "" && a.ResourceVersion == b.ResourceVersion && a.DeletionTimestamp == nil && b.DeletionTimestamp == nil
}

func sameReceiveSet(a, b admittedReceiveSet) bool {
	if !sameReleasedReceiveJournals(a.ReleasedJournals, b.ReleasedJournals) || !sameReceiveRevision(a.Metadata, b.Metadata) || len(a.Journals) != len(b.Journals) || len(a.Local) != len(b.Local) {
		return false
	}
	for uid, snapshot := range a.Journals {
		other, found := b.Journals[uid]
		if !found || other.ContinuesExecution(snapshot) != nil {
			return false
		}
	}
	if len(a.Retirements) != len(b.Retirements) {
		return false
	}
	for uid, before := range a.Retirements {
		after, found := b.Retirements[uid]
		if !found || !sameReceiveRetirement(before, after) {
			return false
		}
	}
	for uid, member := range a.Local {
		other, found := b.Local[uid]
		if !found || other.NodeUID != member.NodeUID || other.BootID != member.BootID {
			return false
		}
	}
	return true
}

type receiveInventoryCheckpoint struct {
	Version      int                     `json:"version"`
	InventoryUID string                  `json:"inventoryUID"`
	Inventory    tunnel.ReceiveInventory `json:"inventory"`
	NodeName     string                  `json:"nodeName"`
	NodeUID      string                  `json:"nodeUID"`
	BootID       string                  `json:"bootID"`
	PublicKey    string                  `json:"publicKey"`
}

// Caller holds the shared native lock. Persist the inventory identity and every
// locally relevant journal before native work can act on their combined routes.
// Inventory shrinkage is not an authorized cleanup protocol.
func checkpointReceiveSetLocked(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, lockPath string, admitted admittedReceiveSet) error {
	target, found := admitted.Local[scope.MachineUID]
	if !found {
		return fmt.Errorf("receiver checkpoint set has no local target")
	}
	path := lockPath + ".inventory.json"
	next := receiveInventoryCheckpoint{Version: 1, InventoryUID: string(admitted.Metadata.UID), Inventory: admitted.Inventory, NodeName: cfg.nodeName, NodeUID: target.NodeUID, BootID: target.BootID, PublicKey: key.PublicKey().String()}
	released, err := admitReleasedReceiveCheckpoints(ctx, api, store, scope, cfg, key, lockPath, next, admitted.Metadata)
	if err != nil {
		return err
	}
	if err := validateReceiveInventoryCheckpointWithRelease(path, next, released); err != nil {
		return err
	}
	var sharedReleased *receiveReleasedCheckpointAdmission
	if len(released.records) > 0 {
		sharedReleased = &released
	}
	// Retirement checkpoints retain withdrawn local histories and fence old
	// boot recovery before active histories can be refreshed.
	for uid, retired := range admitted.Retirements {
		retiredStore, retiredScope := store, scope
		retiredStore.Name, retiredScope.MachineUID = tunnel.ReceiveTransitionName(uid), uid
		retiredStore.ExpectedUID = string(retired.Journals[uid].Metadata().UID)
		if err := checkpointReceiveRetirementWithReleasedLocked(ctx, api, retiredStore, retiredScope, cfg, key, lockPath, retired, sharedReleased); err != nil {
			return err
		}
	}
	// Also preserve journals from an interrupted first checkpoint of the set.
	entries, err := os.ReadDir(filepath.Dir(lockPath))
	if err != nil {
		return err
	}
	expected := released.localFiles
	for uid := range admitted.Local {
		expected[filepath.Base(lockPath)+"."+tunnel.ReceiveTransitionName(uid)+".json"] = true
	}
	for _, retired := range admitted.Retirements {
		for _, plan := range retired.Plans {
			memberScope := scope
			memberScope.MachineUID = plan.MachineUID
			if _, err := receiveJournalParticipant(plan, memberScope, cfg.nodeName, key); err == nil {
				expected[filepath.Base(lockPath)+"."+tunnel.ReceiveTransitionName(plan.MachineUID)+".json"] = true
			}
		}
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(lockPath)+".receive-transition-") && !expected[entry.Name()] {
			return fmt.Errorf("receiver inventory omitted a checkpointed local journal")
		}
	}
	// Each local journal is re-admitted against the current live Node boot
	// below before the inventory checkpoint adopts that boot. An interrupted
	// rewrite may contain both boots; history and inventory identity still fence it.
	for _, uid := range admitted.Inventory.MachineUIDs {
		member, local := admitted.Local[uid]
		if !local {
			continue
		}
		currentStore, currentScope := store, scope
		currentStore.Name, currentScope.MachineUID = tunnel.ReceiveTransitionName(uid), uid
		currentStore.ExpectedUID = string(member.Snapshot.Metadata().UID)
		saved, err := checkpointReceiveJournalLocked(ctx, api, currentStore, currentScope, cfg, key, lockPath+"."+currentStore.Name+".json")
		if err != nil {
			return err
		}
		if saved.Snapshot.ContinuesExecution(member.Snapshot) != nil || saved.NodeUID != member.NodeUID || saved.BootID != member.BootID {
			return fmt.Errorf("receiver set changed while checkpointing")
		}
	}
	saved := receiveInventoryCheckpoint{Version: 1, InventoryUID: string(admitted.Metadata.UID), Inventory: admitted.Inventory, NodeName: cfg.nodeName, NodeUID: target.NodeUID, BootID: target.BootID, PublicKey: key.PublicKey().String()}
	if err := released.recheck(ctx, api, store); err != nil {
		return err
	}
	return writeDurableJSON(path, saved)
}

// Shared by phase and retirement checkpoint writers under the host lock.
func validateReceiveInventoryCheckpoint(path string, next receiveInventoryCheckpoint) error {
	return validateReceiveInventoryCheckpointWithRelease(path, next, receiveReleasedCheckpointAdmission{})
}

func validateReceiveInventoryCheckpointWithRelease(path string, next receiveInventoryCheckpoint, released receiveReleasedCheckpointAdmission) error {
	if len(released.records) > 0 {
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if released.nextSHA != tunnel.HashPeerList(raw) {
			return fmt.Errorf("released checkpoint admission differs from write")
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		var saved receiveInventoryCheckpoint
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&saved); err != nil {
			return err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return fmt.Errorf("trailing receiver inventory checkpoint data")
		}
		a, b := saved.Inventory, next.Inventory
		if saved.Version != 1 || saved.InventoryUID != next.InventoryUID || saved.NodeName != next.NodeName || saved.NodeUID != next.NodeUID || saved.BootID == "" || saved.PublicKey != next.PublicKey || a.Version != b.Version || a.ClusterUID != b.ClusterUID || a.Namespace != b.Namespace || a.MeshName != b.MeshName || a.MeshUID != b.MeshUID || a.MachineUIDs == nil {
			return fmt.Errorf("receiver inventory checkpoint identity changed")
		}
		for uid, retired := range a.Released {
			// Retired identities remain fenced even after their active routing
			// reservation is gone. A later boot must not erase this history or
			// reinterpret it as a fresh publication of the old worker.
			current, found := b.Released[uid]
			_, published := b.Publications[uid]
			_, previouslyPublished := a.Publications[uid]
			if uid == "" || retired.JournalUID == "" || len(retired.Plan) != 0 || len(retired.Retirement) == 0 || len(retired.RetirementCompletion) == 0 || len(retired.RetirementFinalization) == 0 || previouslyPublished || slices.Contains(a.MachineUIDs, uid) || !found || !reflect.DeepEqual(retired, current) || published || slices.Contains(b.MachineUIDs, uid) {
				return fmt.Errorf("released receiver ownership regressed, changed or reappeared")
			}
		}
		previous := ""
		for uid, p := range a.Publications {
			if _, authorized := released.records[uid]; authorized && sameReceiveReleasedFence(p, b.Released[uid]) {
				continue
			}
			if p.JournalUID != "" && b.Publications[uid].JournalUID != p.JournalUID {
				return fmt.Errorf("journal publication regressed or changed incarnation")
			}
			if len(p.Retirement) > 0 && !bytes.Equal(p.Retirement, b.Publications[uid].Retirement) {
				return fmt.Errorf("receiver retirement intent regressed or changed")
			}
			// Once consumed, completion is historical evidence, not a renewable
			// receipt. Preserve both records across later reconciliations and
			// boots; accepting their removal would erase the durable fence.
			if len(p.RetirementFinalization) > 0 && (!bytes.Equal(p.RetirementFinalization, b.Publications[uid].RetirementFinalization) || !bytes.Equal(p.RetirementCompletion, b.Publications[uid].RetirementCompletion)) {
				return fmt.Errorf("receiver retirement finalization regressed or consumed evidence changed")
			}
		}
		for _, uid := range a.MachineUIDs {
			_, authorized := released.records[uid]
			releasedExactly := authorized && sameReceiveReleasedFence(a.Publications[uid], b.Released[uid])
			if uid == "" || uid <= previous || (!slices.Contains(b.MachineUIDs, uid) && !releasedExactly) {
				return fmt.Errorf("receiver inventory regressed or checkpoint is invalid")
			}
			previous = uid
		}
	}
	return nil
}
