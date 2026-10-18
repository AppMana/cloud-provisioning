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
func executeReceiveRetirement(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, containerID string, native receiveRetirementNative) ([]handover.PodReceipt, error) {
	if api == nil || native == nil || containerID == "" || cfg.receiptPodName == "" || cfg.receiptPodUID == "" || scope.MachineUID == "" || store.Name != tunnel.ReceiveTransitionName(scope.MachineUID) {
		return nil, fmt.Errorf("retirement execution requires native verification and agent identity")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock, err := lockReconciliation(cfg)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	admitted, err := loadReceiveRetirement(ctx, api, store, scope, cfg.nodeName, key)
	if err != nil {
		return nil, fmt.Errorf("initial admission: %w", err)
	}
	receipt := handover.Receipt{NodeUID: admitted.NodeUID, ObservedBootID: admitted.BootID, AgentPodName: cfg.receiptPodName, AgentPodUID: cfg.receiptPodUID, AgentContainerName: "dialer", AgentContainerID: containerID, ObservedAt: time.Now()}
	verifyAgent := func() error {
		return handover.VerifyObservationIncarnation(ctx, api, scope.Namespace, cfg.nodeName, receipt)
	}
	if err := verifyAgent(); err != nil {
		return nil, err
	}
	if err := checkpointReceiveRetirementLocked(ctx, api, store, scope, cfg, key, lock.Name(), admitted); err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	binding, err := attachment.NewReceiveRetirementReceiptScope(ctx, store.ConfigMaps, store.ClusterUID, string(admitted.Metadata.UID), admitted.Journals[scope.MachineUID], admitted.Mesh)
	if err != nil {
		return nil, fmt.Errorf("observation binding: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := recheckReceiveRetirement(ctx, api, store, admitted, cfg.nodeName, key); err != nil {
		return nil, fmt.Errorf("pre-cleanup admission: %w", err)
	}
	if err := verifyAgent(); err != nil {
		return nil, err
	}
	if err := native(ctx, lock.Name(), admitted); err != nil {
		return nil, fmt.Errorf("native cleanup: %w", err)
	}
	receipt.ObservedAt = time.Now()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, err := loadReceiveRetirement(ctx, api, store, scope, cfg.nodeName, key)
	if err != nil {
		return nil, fmt.Errorf("post-cleanup admission: %w", err)
	}
	if !sameReceiveRetirement(admitted, current) {
		return nil, fmt.Errorf("retirement changed during native cleanup")
	}
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
