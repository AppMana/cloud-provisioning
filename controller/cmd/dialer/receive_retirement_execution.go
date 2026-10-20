package main

import (
	"context"
	"fmt"
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
	finalized, err := receiveRetirementFinalized(ctx, api, store, scope, admitted)
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
	if err := checkpointReceiveRetirementLocked(ctx, api, store, scope, cfg, key, lock.Name(), admitted); err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	timing.step("binding")
	binding, err := attachment.NewReceiveRetirementReceiptScope(ctx, store.ConfigMaps, store.ClusterUID, string(admitted.Metadata.UID), admitted.Journals[scope.MachineUID], admitted.Mesh)
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
	for _, uid := range binding.CleanupMachineUIDs() {
		member := admitted
		memberStore, memberScope := retirementMemberScope(store, scope, admitted, uid)
		if uid != scope.MachineUID {
			member, err = loadReceiveRetirement(ctx, api, memberStore, memberScope, cfg.nodeName, key)
			if err != nil {
				return nil, fmt.Errorf("joint admission: %w", err)
			}
			if member.NodeUID != admitted.NodeUID || member.BootID != admitted.BootID {
				return nil, fmt.Errorf("joint retirement observer changed")
			}
			if err := checkpointReceiveRetirementLocked(ctx, api, memberStore, memberScope, cfg, key, lock.Name(), member); err != nil {
				return nil, fmt.Errorf("joint checkpoint: %w", err)
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
	var current admittedReceiveRetirement
	for _, member := range members {
		uid := member.Withdrawal.MachineUID
		memberStore, memberScope := retirementMemberScope(store, scope, member, uid)
		refreshed, err := loadReceiveRetirement(ctx, api, memberStore, memberScope, cfg.nodeName, key)
		if err != nil {
			return nil, fmt.Errorf("post-cleanup admission: %w", err)
		}
		if !sameReceiveRetirement(member, refreshed) {
			return nil, fmt.Errorf("retirement changed during native cleanup")
		}
		if uid == scope.MachineUID {
			current = refreshed
		}
	}
	if finalized {
		verified, err := receiveRetirementFinalized(ctx, api, store, scope, current)
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
			return nil, ctx.Err()
		}
	}
	timing.step("post-native-binding")
	refreshed, err := attachment.NewReceiveRetirementReceiptScope(ctx, store.ConfigMaps, store.ClusterUID, string(current.Metadata.UID), current.Journals[scope.MachineUID], current.Mesh)
	if err != nil {
		return nil, fmt.Errorf("post-cleanup binding: %w", err)
	}
	if refreshed.Digest() != binding.Digest() {
		return nil, fmt.Errorf("retirement observation binding changed")
	}
	if err := verifyAgent(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	receipt.RoundHash = binding.Digest()
	return []handover.PodReceipt{{BootID: admitted.BootID, Receipt: receipt}}, nil
}

// Select the member's immutable journal identity for existing native backends.
func retirementMemberScope(store handover.Store, scope receiveJournalScope, admitted admittedReceiveRetirement, uid string) (handover.Store, receiveJournalScope) {
	store.Name = tunnel.ReceiveTransitionName(uid)
	store.ExpectedUID = string(admitted.Journals[uid].Metadata().UID)
	scope.MachineUID = uid
	return store, scope
}
