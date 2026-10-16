package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"k8s.io/client-go/kubernetes"
)

// This records admitted intent, not completed native work. Recovery cannot
// issue a receipt, infer a completed switch, or advance an expired phase.
type receiveJournalCheckpoint struct {
	Version    int                 `json:"version"`
	Scope      receiveJournalScope `json:"scope"`
	ClusterUID string              `json:"clusterUID"`
	NodeName   string              `json:"nodeName"`
	NodeUID    string              `json:"nodeUID"`
	BootID     string              `json:"bootID"`
	PublicKey  string              `json:"publicKey"`
	Journal    json.RawMessage     `json:"journal"`
}

type recoveredReceiveJournal struct {
	Snapshot handover.Snapshot
	Plan     tunnel.ReceiveTransitionPlan
}

func readReceiveJournalCheckpoint(path string, store handover.Store, scope receiveJournalScope, nodeName string, key wgtypes.Key) (recoveredReceiveJournal, error) {
	fail := func(err error) (recoveredReceiveJournal, error) { return recoveredReceiveJournal{}, err }
	raw, err := os.ReadFile(path)
	if err != nil {
		return fail(err)
	}
	var saved receiveJournalCheckpoint
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&saved); err != nil {
		return fail(err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return fail(fmt.Errorf("trailing receiver checkpoint data"))
	}
	boot, err := localReceiveBootID()
	if err != nil {
		return fail(err)
	}
	if saved.Version != 1 || saved.Scope != scope || saved.ClusterUID != store.ClusterUID || saved.NodeName != nodeName || saved.NodeUID == "" || saved.BootID != boot || key == (wgtypes.Key{}) || saved.PublicKey != key.PublicKey().String() {
		return fail(fmt.Errorf("receiver checkpoint identity or boot changed"))
	}
	snapshot, err := store.RestoreCheckpoint(saved.Journal)
	if err != nil {
		return fail(err)
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	if err != nil {
		return fail(err)
	}
	uid, err := receiveJournalParticipant(plan, scope, nodeName, key)
	if err != nil {
		return fail(err)
	}
	if uid != saved.NodeUID {
		return fail(fmt.Errorf("receiver checkpoint participant changed"))
	}
	return recoveredReceiveJournal{Snapshot: snapshot, Plan: plan}, nil
}

// checkpointReceiveJournal serializes admission and durable recording with the
// existing bootstrap/pod reconciliation lock. It is not a native phase executor.
// Corrupt or superseded recovery state blocks admission rather than falling back
// to the legacy peer cache. A new boot requires explicit live recovery before
// replacing the checkpoint; it cannot reuse an old native observation.
func checkpointReceiveJournal(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key) (admittedReceiveJournal, error) {
	fail := func(err error) (admittedReceiveJournal, error) { return admittedReceiveJournal{}, err }
	if scope.MachineUID == "" || store.Name != tunnel.ReceiveTransitionName(scope.MachineUID) {
		return fail(fmt.Errorf("invalid receiver checkpoint machine"))
	}
	lock, err := lockReconciliation(cfg)
	if err != nil {
		return fail(err)
	}
	defer lock.Close()
	path := lock.Name() + "." + store.Name + ".json"
	previous, readErr := readReceiveJournalCheckpoint(path, store, scope, cfg.nodeName, key)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fail(readErr)
	}
	admitted, err := loadReceiveJournal(ctx, api, store, scope, cfg.nodeName, key)
	if err != nil {
		return fail(err)
	}
	if readErr == nil {
		if err := admitted.Snapshot.Extends(previous.Snapshot); err != nil {
			return fail(err)
		}
	}
	raw, err := admitted.Snapshot.Checkpoint()
	if err != nil {
		return fail(err)
	}
	saved := receiveJournalCheckpoint{Version: 1, Scope: scope, ClusterUID: store.ClusterUID, NodeName: cfg.nodeName, NodeUID: admitted.NodeUID, BootID: admitted.BootID, PublicKey: key.PublicKey().String(), Journal: raw}
	if err := writeDurableJSON(path, saved); err != nil {
		return fail(err)
	}
	return admitted, nil
}
