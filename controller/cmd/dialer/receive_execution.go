package main

import (
	"context"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"k8s.io/client-go/kubernetes"
)

// receiveNativePhase must apply the exact committed direction and independently
// read back every local member's devices, routes and phase conditions. Returning
// nil asserts that native verification completed; merely accepting configuration
// is insufficient. The combined route intent includes other workers and must
// preserve their selection; only the target plan authorizes phase-specific
// preparation or retirement. Completed phase verification is read-only. An
// explicit maintenance callback may restore the selected transport and prepare
// an independently admitted additive successor, but never switch to it, replay
// retirement or issue phase receipts. Platforms must reject unsupported phases.
type receiveNativePhase func(context.Context, tunnel.ReceiveTransitionPlan, handover.View, string, tunnel.ReceiveJournalRoutes) error

// executeReceivePhase holds the existing host lock from recovery admission to
// native verification and receipt construction. It is an execution boundary,
// not a native backend or receipt transport. Callers must not call it while
// already holding the same lock.
//
// A changed journal after native work suppresses receipts; it does not undo
// that work or infer rollback. The next pass must admit the newer committed
// history. Every other writer must honor the same lock and durable phase state.
func executeReceivePhase(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, containerID string, native receiveNativePhase) ([]handover.PodReceipt, error) {
	return runReceivePhase(ctx, api, store, scope, cfg, key, containerID, native, false, false)
}

// Maintenance uses the same live admission, complete-set checkpoint, host lock
// and post-action fencing as verification, but never emits phase receipts.
func maintainCompletedReceivePhase(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, containerID string, native receiveNativePhase) error {
	_, err := runReceivePhase(ctx, api, store, scope, cfg, key, containerID, native, true, true)
	return err
}

func verifyCompletedReceivePhase(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, containerID string, native receiveNativePhase) error {
	_, err := runReceivePhase(ctx, api, store, scope, cfg, key, containerID, native, true, false)
	return err
}

func runReceivePhase(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, containerID string, native receiveNativePhase, completedReadback, completedMaintenance bool) (result []handover.PodReceipt, resultErr error) {
	operation := "phase"
	if completedMaintenance {
		operation = "maintenance"
	} else if completedReadback {
		operation = "readback"
	}
	timing := newReceiveTiming(cfg.receiptURL != "", operation)
	defer func() { timing.finish(resultErr == nil) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if native == nil || containerID == "" || scope.MachineUID == "" || store.Name != tunnel.ReceiveTransitionName(scope.MachineUID) {
		return nil, fmt.Errorf("phase execution requires native verification and admitted identities")
	}
	timing.step("lock")
	lock, err := lockReconciliation(cfg)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	timing.step("admission")
	set, err := loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key)
	if err != nil {
		return nil, err
	}
	timing.step("checkpoint")
	if err := checkpointReceiveSetLocked(ctx, api, store, scope, cfg, key, lock.Name(), set); err != nil {
		return nil, err
	}
	timing.step("phase-authority")
	admitted := set.Local[scope.MachineUID]
	view, err := admitted.Snapshot.View()
	if err != nil {
		return nil, err
	}
	if completedReadback {
		if !view.Complete {
			return nil, fmt.Errorf("completed readback requires a terminal journal")
		}
	} else {
		view, err = executableReceiveRound(admitted.Snapshot, time.Now())
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if view.Round.Phase == handover.Drain && (view.DrainUntil.IsZero() || time.Now().Before(view.DrainUntil)) {
		return nil, fmt.Errorf("native drain verification cannot start before committed drain deadline")
	}
	deadline := view.Round.ExpiresAt
	if completedReadback {
		deadline = time.Now().Add(30 * time.Second)
	}
	for _, member := range set.Local {
		if completedReadback && !completedMaintenance {
			break
		} // Readback cannot extend another worker's mutation authority.
		other, err := member.Snapshot.View()
		if err != nil {
			return nil, err
		}
		// Reapplying combined routes can affect another local worker. Its
		// unfinished intent cannot authorize new native work after expiry.
		if other.Complete {
			continue
		}
		if time.Now().Before(other.Round.IssuedAt) || !time.Now().Before(other.Round.ExpiresAt) {
			return nil, fmt.Errorf("another local handover round is not currently executable")
		}
		if other.Round.ExpiresAt.Before(deadline) {
			deadline = other.Round.ExpiresAt
		}
	}
	phaseCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	timing.step("pre-native-admission")
	if err := recheckReceiveSet(phaseCtx, api, store, scope, cfg.nodeName, key, set); err != nil {
		return nil, err
	}
	if err := phaseCtx.Err(); err != nil {
		return nil, err
	}
	checkRelay := func(owner tunnel.ReceiveTransitionSite, receipt handover.Receipt) error {
		return handover.VerifyObservationIncarnation(phaseCtx, api, scope.Namespace, owner.NodeName, receipt)
	}
	checkDependencies := func(routes tunnel.ReceiveJournalRoutes) error {
		if completedMaintenance {
			for uid := range set.Local {
				other := routes.Views[uid]
				if !other.Complete && other.Round.Phase == handover.Switch {
					if err := visitReceiveRelayDependencies(routes, admitted.NodeUID, uid, other, time.Now(), checkRelay); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if completedReadback || view.Round.Phase != handover.Switch {
			return nil
		}
		return visitReceiveRelayDependencies(routes, admitted.NodeUID, scope.MachineUID, view, time.Now(), checkRelay)
	}
	if err := checkDependencies(set.Routes); err != nil {
		return nil, err
	}
	timing.step("native")
	if err := native(phaseCtx, admitted.Plan, view, admitted.NodeUID, set.Routes); err != nil {
		return nil, err
	}
	observed := time.Now()
	if err := phaseCtx.Err(); err != nil {
		return nil, err
	}
	// Re-read live object identities and journal execution authority. Native
	// execution can outlast a phase, deletion, replacement or boot transition.
	timing.step("post-native-admission")
	afterNative, err := loadReceiveSet(phaseCtx, api, store, scope, cfg.nodeName, key)
	if err != nil {
		return nil, err
	}
	if !sameReceiveSet(set, afterNative) {
		return nil, fmt.Errorf("handover changed during native phase execution")
	}
	if err := checkDependencies(afterNative.Routes); err != nil {
		return nil, err
	}
	current := afterNative.Local[scope.MachineUID]
	if err := phaseCtx.Err(); err != nil {
		return nil, err
	}
	timing.step("observation-checkpoint")
	if completedReadback {
		if cfg.peersFile != "" && current.NodeUID == current.Plan.CloudNodeUID {
			if err := checkpointReceiveNativeObservation(phaseCtx, api, store, scope, cfg, key, lock.Name(), containerID, observed, afterNative); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	now := time.Now()
	view, err = executableReceiveRound(current.Snapshot, now)
	if err != nil {
		return nil, err
	}
	if now.Sub(observed) > view.Round.MaxReceiptAge {
		return nil, fmt.Errorf("native observation aged out during identity revalidation")
	}
	if view.Round.Phase == handover.Drain && (view.DrainUntil.IsZero() || observed.Before(view.DrainUntil)) {
		return nil, fmt.Errorf("native drain verification precedes committed drain deadline")
	}
	digest, err := view.Round.Digest()
	if err != nil {
		return nil, err
	}
	var receipts []handover.PodReceipt
	for _, member := range view.Round.Members {
		if member.NodeUID != current.NodeUID {
			continue
		}
		receipts = append(receipts, handover.PodReceipt{BootID: current.BootID, Receipt: handover.Receipt{AgentContainerID: containerID, RoundHash: digest, NodeUID: member.NodeUID, DeviceID: member.DeviceID, FromKey: member.FromKey, ToKey: member.ToKey, FromPeerKey: member.FromPeerKey, ToPeerKey: member.ToPeerKey, ObservedAt: observed}})
	}
	if len(receipts) == 0 {
		return nil, fmt.Errorf("native phase has no local members")
	}
	return receipts, nil
}

func executableReceiveRound(snapshot handover.Snapshot, now time.Time) (handover.View, error) {
	view, err := snapshot.View()
	if err != nil {
		return handover.View{}, err
	}
	if view.Complete || now.Before(view.Round.IssuedAt) || !now.Before(view.Round.ExpiresAt) {
		return handover.View{}, fmt.Errorf("handover round is complete, not yet valid or expired")
	}
	return view, nil
}
