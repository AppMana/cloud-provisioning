package main

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"k8s.io/client-go/kubernetes"
)

// Recheck is a guard within one fully admitted operation, never initial
// admission or final verification. Callers retain the native reconciliation lock.
func recheckReceiveSet(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, nodeName string, key wgtypes.Key, admitted admittedReceiveSet) error {
	if len(admitted.Retirements) == 0 {
		current, err := loadReceiveSet(ctx, api, store, scope, nodeName, key)
		if err != nil {
			return err
		}
		if !sameReceiveSet(admitted, current) {
			return fmt.Errorf("receiver set changed during guard")
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	target, found := admitted.Local[scope.MachineUID]
	if !found || store.Name != tunnel.ReceiveTransitionName(scope.MachineUID) || (store.ExpectedUID != "" && store.ExpectedUID != string(target.Snapshot.Metadata().UID)) {
		return fmt.Errorf("receiver guard requires admitted local scope")
	}
	// Serving members still require live adoption, mesh allocation and local
	// participant admission. Reuse only the retained retirement histories.
	for uid, before := range admitted.Local {
		memberStore, memberScope := store, scope
		memberStore.Name, memberStore.ExpectedUID, memberScope.MachineUID = tunnel.ReceiveTransitionName(uid), string(before.Snapshot.Metadata().UID), uid
		current, err := loadReceiveJournal(ctx, api, memberStore, memberScope, nodeName, key)
		if err != nil {
			return err
		}
		if current.NodeUID != before.NodeUID || current.BootID != before.BootID {
			return fmt.Errorf("receiver guard participant changed")
		}
		if err := current.Snapshot.ContinuesExecution(before.Snapshot); err != nil {
			return err
		}
	}
	// Each admitted retirement spans every reserved journal. Recheck it after
	// serving admission so changes during those reads cannot be hidden.
	for uid, retired := range admitted.Retirements {
		if retired.Mesh == nil || retired.Mesh.Namespace != scope.Namespace || retired.Mesh.Name != scope.MeshName || string(retired.Mesh.UID) != scope.MeshUID || retired.Withdrawal.MachineUID != uid || !sameReceiveRevision(admitted.Metadata, retired.Metadata) {
			return fmt.Errorf("receiver guard withdrawal scope changed")
		}
		journal, found := retired.Journals[uid]
		if !found {
			return fmt.Errorf("receiver guard withdrawal journal missing")
		}
		memberStore := store
		memberStore.Name, memberStore.ExpectedUID = tunnel.ReceiveTransitionName(uid), string(journal.Metadata().UID)
		if err := recheckReceiveRetirement(ctx, api, memberStore, retired, nodeName, key); err != nil {
			return err
		}
	}
	return ctx.Err()
}
