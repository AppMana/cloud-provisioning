package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"k8s.io/client-go/kubernetes"
)

// receiveRetirementNative runs under the existing host lock. Success requires
// removal and independent readback of every local obligation: receivers, peers,
// routes, source exceptions, advertisements and executable caches. Unsupported
// consumer roles must fail; partial cleanup must never return success.
type receiveRetirementNative func(context.Context, string, admittedReceiveRetirement) error

// executeReceiveRetirement uses persisted withdrawal authority, not a completed
// serving round. It returns a credential-free observation only after native
// verification and renewed identity/source checks. The controller independently
// authenticates and ages it. Source changes suppress receipts without undoing
// cleanup; retry must admit the current state. Call without holding the host lock.
// Finalized maintenance returns no receipts by default. Explicit release
// observation uses the same cleanup and checkpoints, then emits a fresh bound
// receipt; historical evidence never substitutes for native verification.
func executeReceiveRetirement(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, containerID string, native receiveRetirementNative) (result []handover.PodReceipt, resultErr error) {
	group, err := executeReceiveRetirementCohort(ctx, api, store, scope, cfg, key, containerID, native)
	if err != nil {
		return nil, err
	}
	return group[scope.MachineUID], nil
}

// Execute a cohort once and bind each member's receipt independently. An entry
// with no receipts means that member's finalized maintenance succeeded. Results
// are local to this execution; any member failure suppresses the entire group.
func executeReceiveRetirementCohort(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, containerID string, native receiveRetirementNative) (result map[string][]handover.PodReceipt, resultErr error) {
	timing := newReceiveTiming(cfg.receiptURL != "", "retirement")
	defer func() { timing.finish(resultErr == nil) }()
	if api == nil || native == nil || containerID == "" || cfg.receiptPodName == "" || cfg.receiptPodUID == "" || scope.MachineUID == "" || store.Name != tunnel.ReceiveTransitionName(scope.MachineUID) {
		return nil, fmt.Errorf("retirement execution requires native verification and agent identity")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	timing.step("lock")
	lock, err := lockReconciliation(cfg)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	timing.step("admission")
	admitted, err := loadReceiveRetirement(ctx, api, store, scope, cfg.nodeName, key)
	if err != nil {
		return nil, fmt.Errorf("initial admission: %w", err)
	}
	targetFinalized, err := receiveRetirementFinalized(ctx, api, store, scope, admitted)
	if err != nil {
		return nil, fmt.Errorf("finalization admission: %w", err)
	}
	receipt := handover.Receipt{NodeUID: admitted.NodeUID, ObservedBootID: admitted.BootID, AgentPodName: cfg.receiptPodName, AgentPodUID: cfg.receiptPodUID, AgentContainerName: "dialer", AgentContainerID: containerID, ObservedAt: time.Now()}
	verifyAgent := func() error {
		return handover.VerifyObservationIncarnation(ctx, api, scope.Namespace, cfg.nodeName, receipt)
	}
	timing.step("agent-identity")
	if err := verifyAgent(); err != nil {
		return nil, err
	}
	timing.step("checkpoint")
	if len(admitted.Inventory.Released) > 0 {
		next := receiveInventoryCheckpoint{Version: 1, InventoryUID: string(admitted.Metadata.UID), Inventory: admitted.Inventory, NodeName: cfg.nodeName, NodeUID: admitted.NodeUID, BootID: admitted.BootID, PublicKey: key.PublicKey().String()}
		released, err := admitReleasedReceiveCheckpoints(ctx, api, store, scope, cfg, key, lock.Name(), next, admitted.Metadata)
		if err != nil {
			return nil, fmt.Errorf("released checkpoint admission: %w", err)
		}
		admitted.releasedCheckpoint = &released
	}
	if err := checkpointReceiveRetirementLocked(ctx, api, store, scope, cfg, key, lock.Name(), admitted); err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	timing.step("binding")
	binding, err := admitted.ReceiptScope(ctx, store)
	if err != nil {
		return nil, fmt.Errorf("observation binding: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !binding.RequiresConsumer(admitted.NodeUID) {
		return nil, fmt.Errorf("departing worker cannot observe joint retirement")
	}
	// Admit and checkpoint every member before touching native state. All cleanup
	// shares the host lock and the original deadline; a member gets no fresh budget.
	members := make([]admittedReceiveRetirement, 0, len(binding.CleanupMachineUIDs()))
	bindings := map[string]attachment.ReceiveRetirementReceiptScope{scope.MachineUID: binding}
	finalized := map[string]bool{scope.MachineUID: targetFinalized}
	for _, uid := range binding.CleanupMachineUIDs() {
		member := admitted
		memberStore, memberScope := retirementMemberScope(store, scope, admitted, uid)
		if uid != scope.MachineUID {
			member, err = receiveRetirementMember(memberStore, memberScope, admitted)
			if err != nil {
				return nil, fmt.Errorf("joint admission: %w", err)
			}
			if err := recheckReceiveRetirement(ctx, api, memberStore, member, cfg.nodeName, key); err != nil {
				return nil, fmt.Errorf("joint source or observer admission: %w", err)
			}
			finalized[uid], err = receiveRetirementFinalized(ctx, api, memberStore, memberScope, member)
			if err != nil {
				return nil, fmt.Errorf("joint finalization admission: %w", err)
			}
			if err := checkpointReceiveRetirementLocked(ctx, api, memberStore, memberScope, cfg, key, lock.Name(), member); err != nil {
				return nil, fmt.Errorf("joint checkpoint: %w", err)
			}
			bindings[uid], err = member.ReceiptScope(ctx, memberStore)
			if err != nil {
				return nil, fmt.Errorf("joint observation binding: %w", err)
			}
			if !bindings[uid].RequiresConsumer(admitted.NodeUID) || !slices.Equal(bindings[uid].CleanupMachineUIDs(), binding.CleanupMachineUIDs()) {
				return nil, fmt.Errorf("joint observation membership changed")
			}
		}
		members = append(members, member)
	}
	for i, member := range members {
		timing.step("pre-native-admission")
		memberStore, _ := retirementMemberScope(store, scope, member, member.Withdrawal.MachineUID)
		if err := recheckReceiveRetirement(ctx, api, memberStore, member, cfg.nodeName, key); err != nil {
			return nil, fmt.Errorf("pre-cleanup admission: %w", err)
		}
		if err := verifyAgent(); err != nil {
			return nil, err
		}
		timing.step("native")
		if err := native(ctx, lock.Name(), member); err != nil {
			return nil, fmt.Errorf("native cleanup: %w", err)
		}
		if i == 0 {
			// Joint evidence is only as fresh as its earliest native observation.
			// Later members must not renew the first member's absence timestamp.
			receipt.ObservedAt = time.Now()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	timing.step("post-native-admission")
	// Read the complete set again after native work. Every member is selected
	// from that same fresh admission and independently fenced below, rather
	// than reconstructing all histories again for each member.
	refreshedSet, err := loadReceiveRetirement(ctx, api, store, scope, cfg.nodeName, key)
	if err != nil {
		return nil, fmt.Errorf("post-cleanup admission: %w", err)
	}
	current := make(map[string]admittedReceiveRetirement, len(members))
	for _, member := range members {
		uid := member.Withdrawal.MachineUID
		memberStore, memberScope := retirementMemberScope(store, scope, member, uid)
		refreshed, err := receiveRetirementMember(memberStore, memberScope, refreshedSet)
		if err != nil {
			return nil, fmt.Errorf("post-cleanup admission: %w", err)
		}
		if !sameReceiveRetirement(member, refreshed) {
			return nil, fmt.Errorf("retirement changed during native cleanup")
		}
		if err := recheckReceiveRetirement(ctx, api, memberStore, refreshed, cfg.nodeName, key); err != nil {
			return nil, fmt.Errorf("post-cleanup member admission: %w", err)
		}
		current[uid] = refreshed
	}
	result = make(map[string][]handover.PodReceipt, len(members))
	for _, member := range members {
		uid := member.Withdrawal.MachineUID
		memberStore, memberScope := retirementMemberScope(store, scope, member, uid)
		result[uid] = nil
		if finalized[uid] {
			verified, err := receiveRetirementFinalized(ctx, api, memberStore, memberScope, current[uid])
			if err != nil {
				return nil, fmt.Errorf("post-cleanup finalization: %w", err)
			}
			if !verified {
				return nil, fmt.Errorf("retirement finalization disappeared during cleanup")
			}
			if err := verifyAgent(); err != nil {
				return nil, err
			}
			// Release observation must still pass the post-native digest and agent
			// checks below. Never replay the historical completion as a new proof.
			if !cfg.observeFinalizedRetirements {
				continue
			}
		}
		timing.step("post-native-binding")
		refreshed, err := current[uid].ReceiptScope(ctx, memberStore)
		if err != nil {
			return nil, fmt.Errorf("post-cleanup binding: %w", err)
		}
		if refreshed.Digest() != bindings[uid].Digest() {
			return nil, fmt.Errorf("retirement observation binding changed")
		}
		if err := verifyAgent(); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		memberReceipt := receipt
		memberReceipt.RoundHash = bindings[uid].Digest()
		result[uid] = []handover.PodReceipt{{BootID: admitted.BootID, Receipt: memberReceipt}}
	}
	if err := verifyAgent(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// Select the member's immutable journal identity for existing native backends.
func retirementMemberScope(store handover.Store, scope receiveJournalScope, admitted admittedReceiveRetirement, uid string) (handover.Store, receiveJournalScope) {
	store.Name = tunnel.ReceiveTransitionName(uid)
	store.ExpectedUID = string(admitted.Journals[uid].Metadata().UID)
	scope.MachineUID = uid
	return store, scope
}
