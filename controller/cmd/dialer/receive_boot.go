package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type receiveBootState struct {
	Config  config
	Key     wgtypes.Key
	Plan    tunnel.ReceiveTransitionPlan
	View    handover.View
	Routes  tunnel.ReceiveJournalRoutes
	NodeUID string
}

// This restores previously verified terminal state after kernel state was lost
// across a boot. It cannot select a new generation, replay retirement, publish
// receipts or replace live admission. The host's ordinary peer cache stays fenced.
func recoverReceiveBoot(ctx context.Context, devices receiverDevices, cfg config) (bool, error) {
	return admitReceiveBoot(ctx, cfg, func(state receiveBootState, check func() error) error {
		if err := restoreCompletedReceiveState(ctx, devices, state.Config, state.Key, state.Plan, state.View, state.NodeUID, state.Routes); err != nil {
			return err
		}
		groups, err := planSelectedCloudReceivePolicy(state.Config, state.Key, state.Plan, state.View, state.NodeUID, state.Routes)
		if err != nil {
			return err
		}
		if err := restoreReceivePolicyRules(ctx, devices, state.Config, state.Key, groups, check); err != nil {
			return err
		}
		own, allocated, err := tunnel.ReceiveLocalNetwork(state.Plan, state.NodeUID)
		if err != nil {
			return err
		}
		return receiveLinuxPhase(ctx, devices, state.Config, state.Key, own, allocated, state.Plan, state.View, state.NodeUID, state.Routes)
	})
}

func admitReceiveBoot(ctx context.Context, cfg config, native func(receiveBootState, func() error) error) (bool, error) {
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if cfg.peersFile == "" || cfg.secretName != "" || cfg.receiptURL != "" || native == nil {
		return true, fmt.Errorf("boot recovery requires the file-only cloud host service")
	}
	lock, err := lockReconciliation(cfg)
	if err != nil {
		return true, err
	}
	defer lock.Close()
	inventoryPath := lock.Name() + ".inventory.json"
	raw, err := os.ReadFile(inventoryPath)
	if os.IsNotExist(err) {
		return false, nil
	} // The legacy lock still fences any incomplete checkpoint set.
	if err != nil {
		return true, err
	}
	var inventory receiveInventoryCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inventory); err != nil {
		return true, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return true, fmt.Errorf("trailing boot inventory data")
	}
	identityRaw, err := os.ReadFile(cfg.peersFile)
	if err != nil {
		return true, err
	}
	key, allocated, err := receiveDispatchIdentity(cfg)
	if err != nil {
		return true, err
	}
	if inventory.Inventory.Version != 1 || inventory.Inventory.ClusterUID == "" || inventory.Inventory.Namespace == "" || inventory.Inventory.MeshName == "" || inventory.Inventory.MeshUID == "" || inventory.Version != 1 || inventory.InventoryUID == "" || inventory.NodeName == "" || inventory.NodeUID == "" || inventory.BootID == "" || inventory.PublicKey != key.PublicKey().String() || (cfg.nodeName != "" && cfg.nodeName != inventory.NodeName) {
		return true, fmt.Errorf("boot recovery inventory identity differs")
	}
	boot, err := localReceiveBootID()
	if err != nil {
		return true, err
	}
	if boot == inventory.BootID {
		return true, nil
	} // The live agent owns same-boot reconciliation.
	entries, err := os.ReadDir(filepath.Dir(lock.Name()))
	if err != nil {
		return true, err
	}
	var machineUID, journalPath string
	for _, uid := range inventory.Inventory.MachineUIDs {
		candidate := lock.Name() + "." + tunnel.ReceiveTransitionName(uid) + ".json"
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return true, err
		}
		if machineUID != "" {
			return true, fmt.Errorf("boot recovery requires one local cloud journal")
		}
		machineUID, journalPath = uid, candidate
	}
	if machineUID == "" {
		return true, fmt.Errorf("boot recovery lacks its local journal")
	}
	store := handover.Store{ClusterUID: inventory.Inventory.ClusterUID, Name: tunnel.ReceiveTransitionName(machineUID)}
	scope := receiveJournalScope{Namespace: inventory.Inventory.Namespace, MeshName: inventory.Inventory.MeshName, MeshUID: inventory.Inventory.MeshUID, MachineUID: machineUID}
	observationPath := receiveNativeObservationPath(journalPath, store.Name)
	files := map[string][]byte{inventoryPath: raw, cfg.peersFile: identityRaw}
	for _, path := range []string{journalPath, observationPath} {
		content, err := os.ReadFile(path)
		if err != nil {
			return true, err
		}
		files[path] = content
	}
	recovered, err := readReceiveJournalCheckpointForAdmission(journalPath, store, scope, inventory.NodeName, key, true)
	if err != nil {
		return true, err
	}
	observation := recovered.NativeObservation
	if observation == nil || observation.InventorySHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) || observation.BootID != inventory.BootID || !slices.Equal(observation.LocalMachineUIDs, []string{machineUID}) || recovered.Plan.CloudNodeUID != inventory.NodeUID || recovered.Plan.CloudNodeName != inventory.NodeName || recovered.Plan.RootPublicKey != key.PublicKey().String() {
		return true, fmt.Errorf("boot recovery lacks verified complete cloud inventory")
	}
	if err := inventory.Inventory.CheckJournal(machineUID, string(recovered.Snapshot.Metadata().UID)); err != nil {
		return true, err
	}
	if !slices.Equal(allocated, recovered.Plan.CloudAddresses) {
		return true, fmt.Errorf("boot recovery allocation differs from local identity")
	}
	network := observation.Network
	if network == nil || network.Interface != cfg.iface || network.RouteTable <= unix.RT_TABLE_LOCAL || network.FWMark <= 0 || (network.MTU != 0 && (network.MTU < 1280 || network.MTU > 65535)) || network.ListenPort < 0 || network.ListenPort > 65535 || network.KeepaliveSeconds < 0 || network.KeepaliveSeconds > 65535 {
		return true, fmt.Errorf("boot recovery lacks verified network configuration")
	}
	legacyObservationPath := lock.Name() + ".native-" + store.Name + ".json"
	allowed := map[string]bool{filepath.Base(inventoryPath): true, filepath.Base(journalPath): true, filepath.Base(observationPath): true, filepath.Base(legacyObservationPath): true}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(lock.Name())+".") && !allowed[entry.Name()] {
			return true, fmt.Errorf("boot recovery has an unaccounted checkpoint")
		}
	}
	cfg.nodeName = inventory.NodeName
	cfg.routeTable, cfg.fwmark, cfg.mtu = network.RouteTable, network.FWMark, network.MTU
	cfg.listenPort, cfg.keepaliveSecs = network.ListenPort, network.KeepaliveSeconds
	routes, err := tunnel.PlanReceiveJournalRoutes([]handover.Snapshot{recovered.Snapshot}, store.ClusterUID)
	if err != nil {
		return true, err
	}
	view := routes.Views[machineUID]
	if !view.Complete {
		return true, fmt.Errorf("boot recovery cannot advance unfinished intent")
	}
	state := receiveBootState{Config: cfg, Key: key, Plan: recovered.Plan, View: view, Routes: routes, NodeUID: inventory.NodeUID}
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		currentBoot, err := localReceiveBootID()
		if err != nil {
			return err
		}
		if currentBoot != boot {
			return fmt.Errorf("boot changed during recovery")
		}
		for path, expected := range files {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Equal(content, expected) {
				return fmt.Errorf("durable boot authority changed")
			}
		}
		return nil
	}
	if err := check(); err != nil {
		return true, err
	}
	if err := native(state, check); err != nil {
		return true, err
	}
	return true, check()
}
