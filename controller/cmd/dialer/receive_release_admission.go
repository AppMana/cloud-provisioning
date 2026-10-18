package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Caller holds the reconciliation lock. Admission is read-only and does not
// authorize native cleanup or deletion of any retired checkpoint.
type receiveReleasedCheckpointAdmission struct {
	nodeName, nodeUID, bootID string
	nextSHA                   string
	records                   map[string]attachment.ReceiveReleasedJournal
	localFiles                map[string]bool
}

func admitReleasedReceiveCheckpoints(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, lockPath string, next receiveInventoryCheckpoint, metadata metav1.ObjectMeta) (receiveReleasedCheckpointAdmission, error) {
	fail := func(err error) (receiveReleasedCheckpointAdmission, error) {
		return receiveReleasedCheckpointAdmission{}, err
	}
	result := receiveReleasedCheckpointAdmission{records: map[string]attachment.ReceiveReleasedJournal{}, localFiles: map[string]bool{}}
	if len(next.Inventory.Released) == 0 {
		return result, nil
	}
	if api == nil || store.ConfigMaps == nil || key == (wgtypes.Key{}) || next.NodeUID == "" || next.BootID == "" || next.NodeName != cfg.nodeName || next.PublicKey != key.PublicKey().String() || next.InventoryUID != string(metadata.UID) {
		return fail(fmt.Errorf("released checkpoint scope changed"))
	}
	node, err := api.CoreV1().Nodes().Get(ctx, cfg.nodeName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if err := validateReceiveParticipantNode(node, next.NodeUID, next.BootID); err != nil {
		return fail(err)
	}
	boot, err := localReceiveBootID()
	if err != nil {
		return fail(err)
	}
	if boot != next.BootID {
		return fail(fmt.Errorf("released checkpoint boot changed"))
	}
	for uid, p := range next.Inventory.Released {
		retainedConsumer := false
		memberStore, memberScope := store, scope
		memberStore.Name = tunnel.ReceiveTransitionName(uid)
		memberStore.ExpectedUID = p.JournalUID
		memberScope.MachineUID = uid
		snapshot, err := memberStore.Load(ctx)
		if err != nil {
			return fail(err)
		}
		view, err := snapshot.Transition().View()
		if err != nil {
			return fail(err)
		}
		released, verified, err := attachment.ReadReleasedReceiveJournal(ctx, api, memberStore, scope.Namespace, scope.MeshName, scope.MeshUID, uid, next.InventoryUID, view.Round.MaxReceiptAge)
		if err != nil {
			return fail(err)
		}
		if !verified || !sameReceiveRevision(metadata, released.Metadata) || !reflect.DeepEqual(next.Inventory, released.Inventory) {
			return fail(fmt.Errorf("released checkpoint inventory changed during admission"))
		}
		for _, c := range released.Withdrawal.Consumers {
			if c.NodeName != cfg.nodeName && c.NodeUID != next.NodeUID && (c.PublicKey == "" || c.PublicKey != key.PublicKey().String()) {
				continue
			}
			retainedConsumer = true
			if c.NodeName != cfg.nodeName || c.NodeUID != next.NodeUID || (c.PublicKey != key.PublicKey().String() && !(c.Site && c.PublicKey == "")) {
				return fail(fmt.Errorf("released checkpoint consumer identity changed"))
			}
		}

		if retainedConsumer {
			raw, err := readPrivateRetirementCache(lockPath + ".inventory.json")
			if err != nil {
				return fail(err)
			}
			var saved receiveInventoryCheckpoint
			if err := json.Unmarshal(raw, &saved); err != nil {
				return fail(err)
			}
			previous, active := saved.Inventory.Publications[uid]
			if !active {
				previous = saved.Inventory.Released[uid]
			}
			if saved.InventoryUID != next.InventoryUID || saved.NodeName != next.NodeName || saved.NodeUID != next.NodeUID || saved.PublicKey != next.PublicKey || saved.BootID == "" || (!active && !reflect.DeepEqual(previous, p) || active && !sameReceiveReleasedFence(previous, p)) {
				return fail(fmt.Errorf("release requires the exact local retirement fence"))
			}
		}
		if released.Plan.CloudNodeName == cfg.nodeName || released.Plan.CloudNodeUID == next.NodeUID || released.Plan.RootPublicKey == key.PublicKey().String() {
			return fail(fmt.Errorf("retired worker cannot admit survivor release"))
		}
		if localUID, err := receiveJournalParticipant(released.Plan, memberScope, cfg.nodeName, key); err == nil {
			if localUID != next.NodeUID {
				return fail(fmt.Errorf("released local journal identity changed"))
			}
			name := lockPath + "." + memberStore.Name + ".json"
			if _, err := readPrivateRetirementCache(name); err != nil {
				return fail(err)
			}
			saved, err := readReceiveJournalCheckpointForAdmission(name, memberStore, memberScope, cfg.nodeName, key, true)
			if err != nil {
				return fail(err)
			}
			if err := memberStore.CheckHistory(ctx, released.Journal, saved.Snapshot); err != nil {
				return fail(err)
			}
			result.localFiles[filepath.Base(name)] = true
		}
		result.records[uid] = released
	}
	for _, record := range result.records {
		if err := record.Recheck(ctx, api, store); err != nil {
			return fail(err)
		}
	}
	current, err := api.CoreV1().Nodes().Get(ctx, cfg.nodeName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if !sameReceiveParticipantNode(node, current) {
		return fail(fmt.Errorf("released checkpoint participant changed during admission"))
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return fail(err)
	}
	result.nextSHA = tunnel.HashPeerList(raw)
	result.nodeName, result.nodeUID, result.bootID = next.NodeName, next.NodeUID, next.BootID
	return result, ctx.Err()
}

func (r receiveReleasedCheckpointAdmission) recheck(ctx context.Context, api kubernetes.Interface, store handover.Store) error {
	if len(r.records) == 0 {
		return ctx.Err()
	}
	boot, err := localReceiveBootID()
	if err != nil {
		return err
	}
	if boot != r.bootID {
		return fmt.Errorf("released checkpoint local boot changed")
	}
	node, err := api.CoreV1().Nodes().Get(ctx, r.nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := validateReceiveParticipantNode(node, r.nodeUID, r.bootID); err != nil {
		return err
	}
	for _, record := range r.records {
		if err := record.Recheck(ctx, api, store); err != nil {
			return err
		}
	}
	current, err := api.CoreV1().Nodes().Get(ctx, r.nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !sameReceiveParticipantNode(node, current) {
		return fmt.Errorf("released checkpoint participant changed at write boundary")
	}
	return ctx.Err()
}

// Native retirement receipts already require this withdrawal fence to be
// durable. Renewable completion may have changed afterward; a finalization
// already observed locally is immutable and must match the consumed record.
func sameReceiveReleasedFence(previous, released tunnel.ReceiveJournalPublication) bool {
	return previous.JournalUID != "" && previous.JournalUID == released.JournalUID && len(previous.Plan) == 0 && len(previous.Retirement) > 0 && bytes.Equal(previous.Retirement, released.Retirement) && (len(previous.RetirementFinalization) == 0 || bytes.Equal(previous.RetirementFinalization, released.RetirementFinalization) && bytes.Equal(previous.RetirementCompletion, released.RetirementCompletion))
}
