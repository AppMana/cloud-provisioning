package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"k8s.io/client-go/kubernetes"
)

// The journal records admitted intent. An optional historical native observation
// records separate successful readback; neither authorizes phase advancement
// or receipt publication during recovery.
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

// Historical local evidence, never a receipt or authorization to execute a
// phase. A changed checkpoint or boot cannot inherit this observation.
type receiveRecoveryNetwork struct {
	Interface        string `json:"interface"`
	RouteTable       int    `json:"routeTable"`
	FWMark           int    `json:"fwmark"`
	MTU              int    `json:"mtu"`
	ListenPort       int    `json:"listenPort"`
	KeepaliveSeconds int    `json:"keepaliveSeconds"`
}

type receiveNativeObservation struct {
	InventorySHA256  string                  `json:"inventorySHA256,omitempty"`
	LocalMachineUIDs []string                `json:"localMachineUIDs,omitempty"`
	Network          *receiveRecoveryNetwork `json:"network,omitempty"`
	JournalSHA256    string                  `json:"journalSHA256"`
	BootID           string                  `json:"bootID"`
	AgentPodUID      string                  `json:"agentPodUID"`
	AgentContainerID string                  `json:"agentContainerID"`
	ObservedAt       time.Time               `json:"observedAt"`
}

type recoveredReceiveJournal struct {
	Snapshot          handover.Snapshot
	Plan              tunnel.ReceiveTransitionPlan
	NativeObservation *receiveNativeObservation
}

func readReceiveJournalCheckpoint(path string, store handover.Store, scope receiveJournalScope, nodeName string, key wgtypes.Key) (recoveredReceiveJournal, error) {
	return readReceiveJournalCheckpointForAdmission(path, store, scope, nodeName, key, false)
}

// Live admission and verified terminal boot recovery may examine earlier-boot
// intent. The general offline reader stays boot-strict; decoding alone never
// authorizes native work.
func readReceiveJournalCheckpointForAdmission(path string, store handover.Store, scope receiveJournalScope, nodeName string, key wgtypes.Key, allowPreviousBoot bool) (recoveredReceiveJournal, error) {
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
	if saved.Version != 1 || saved.Scope != scope || saved.ClusterUID != store.ClusterUID || saved.NodeName != nodeName || saved.NodeUID == "" || (saved.BootID == "" || (!allowPreviousBoot && saved.BootID != boot)) || key == (wgtypes.Key{}) || saved.PublicKey != key.PublicKey().String() {
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
	observation, err := readReceiveNativeObservation(path, store.Name, saved, snapshot)
	if err != nil {
		return fail(err)
	}
	return recoveredReceiveJournal{Snapshot: snapshot, Plan: plan, NativeObservation: observation}, nil
}

// checkpointReceiveJournal serializes admission and durable recording with the
// existing bootstrap/pod reconciliation lock. It is not a native phase executor.
// Corrupt or superseded recovery state blocks admission rather than falling back
// to the legacy peer cache. A new boot requires successful live admission and
// extending journal history before replacement. A generation boundary also
// requires the complete pinned archive chain; old observations remain fenced.
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
	return checkpointReceiveJournalLocked(ctx, api, store, scope, cfg, key, lock.Name()+"."+store.Name+".json")
}

// Caller holds the native reconciliation lock through any subsequent action.
func checkpointReceiveJournalLocked(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, path string) (admittedReceiveJournal, error) {
	fail := func(err error) (admittedReceiveJournal, error) { return admittedReceiveJournal{}, err }
	previous, readErr := readReceiveJournalCheckpointForAdmission(path, store, scope, cfg.nodeName, key, true)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fail(readErr)
	}
	admitted, err := loadReceiveJournal(ctx, api, store, scope, cfg.nodeName, key)
	if err != nil {
		return fail(err)
	}
	if readErr == nil {
		if err := store.CheckHistory(ctx, admitted.Snapshot, previous.Snapshot); err != nil {
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

// Caller retains the executor lock after native verification and live set
// revalidation. Boot recovery separately admits the complete persisted binding.
func checkpointReceiveNativeObservation(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, lockPath, containerID string, observed time.Time, set admittedReceiveSet) error {
	admitted := set.Local[scope.MachineUID]
	view, err := admitted.Snapshot.Transition().View()
	if err != nil || !view.Complete || admitted.Plan.CloudNodeUID != admitted.NodeUID || admitted.Plan.RootPublicKey != key.PublicKey().String() {
		return fmt.Errorf("native recovery observation requires a completed cloud journal")
	}
	observation := handover.Receipt{NodeUID: admitted.NodeUID, ObservedBootID: admitted.BootID, AgentPodName: cfg.receiptPodName, AgentPodUID: cfg.receiptPodUID, AgentContainerName: "dialer", AgentContainerID: containerID, ObservedAt: observed}
	if err := handover.VerifyObservationIncarnation(ctx, api, scope.Namespace, cfg.nodeName, observation); err != nil {
		return err
	}
	raw, err := admitted.Snapshot.Checkpoint()
	if err != nil {
		return err
	}
	inventory := receiveInventoryCheckpoint{Version: 1, InventoryUID: string(set.Metadata.UID), Inventory: set.Inventory, NodeName: cfg.nodeName, NodeUID: admitted.NodeUID, BootID: admitted.BootID, PublicKey: key.PublicKey().String()}
	expected, err := json.Marshal(inventory)
	if err != nil {
		return err
	}
	inventoryRaw, err := os.ReadFile(lockPath + ".inventory.json")
	if err != nil {
		return err
	}
	if !bytes.Equal(expected, inventoryRaw) {
		return fmt.Errorf("native observation inventory differs from durable admission")
	}
	var local []string
	for uid := range set.Local {
		local = append(local, uid)
	}
	slices.Sort(local)
	saved := receiveNativeObservation{InventorySHA256: fmt.Sprintf("%x", sha256.Sum256(inventoryRaw)), LocalMachineUIDs: local, Network: &receiveRecoveryNetwork{Interface: cfg.iface, RouteTable: cfg.routeTable, FWMark: cfg.fwmark, MTU: cfg.mtu, ListenPort: cfg.listenPort, KeepaliveSeconds: cfg.keepaliveSecs}, JournalSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), BootID: admitted.BootID, AgentPodUID: cfg.receiptPodUID, AgentContainerID: containerID, ObservedAt: observed}
	if err := ctx.Err(); err != nil {
		return err
	}
	path := lockPath + "." + store.Name + ".json"
	observationPath := receiveNativeObservationPath(path, store.Name)
	var endpoints map[string]string
	if cfg.receiveRecoveryDevices != nil {
		endpoints, err = captureReceiveBootEndpoints(cfg.receiveRecoveryDevices, cfg, set.Routes, admitted.NodeUID, time.Now())
		if err != nil {
			return err
		}
	}
	if err := writeDurableJSON(observationPath, saved); err != nil {
		return err
	}
	if cfg.receiveRecoveryDevices != nil {
		return writeReceiveBootEndpoints(observationPath, endpoints)
	}
	return nil
}

// Keep the strict journal checkpoint format unchanged for older agents. The
// separate filename is outside the inventory's reserved journal-file prefix,
// while still falling under the shared lock's legacy-writer fence. Use a new
// observation filename so agents with the older strict observation schema can
// still roll back without trying to decode these additional boot bindings.
func receiveNativeObservationPath(path, journalName string) string {
	return strings.TrimSuffix(path, "."+journalName+".json") + ".native-v2-" + journalName + ".json"
}

func readReceiveNativeObservation(path, journalName string, saved receiveJournalCheckpoint, snapshot handover.Snapshot) (*receiveNativeObservation, error) {
	raw, err := os.ReadFile(receiveNativeObservationPath(path, journalName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var observation receiveNativeObservation
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observation); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("trailing native observation data")
	}
	// A new checkpoint invalidates previous evidence without deleting it. This
	// also makes an interrupted journal/observation update fail closed.
	if observation.JournalSHA256 != fmt.Sprintf("%x", sha256.Sum256(saved.Journal)) || observation.BootID != saved.BootID {
		return nil, nil
	}
	view, err := snapshot.Transition().View()
	if err != nil || !view.Complete || observation.AgentPodUID == "" || observation.AgentContainerID == "" || observation.ObservedAt.IsZero() || observation.ObservedAt.After(time.Now()) {
		return nil, fmt.Errorf("receiver native observation differs from terminal intent")
	}
	return &observation, nil
}
