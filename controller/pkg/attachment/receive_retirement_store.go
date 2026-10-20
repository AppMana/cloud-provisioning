package attachment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

type receiveWithdrawalRecord struct {
	Version       int                    `json:"version"`
	JournalSHA256 string                 `json:"journalSHA256"`
	Withdrawal    RemoteWithdrawalIntent `json:"withdrawal"`
	// Exact public transport fields removed by this withdrawal. Version two
	// retains them only to validate sibling captures, never for native replay.
	PeerData map[string][]byte `json:"peerData,omitempty"`
}

// ReserveReceiveWithdrawal persists the captured consumers before peer removal.
// The caller establishes drain authority and freezes the journal first. Inventory
// CAS preserves other workers; the journal UID and fenced history remain bound
// across retries. For ordinary removal this does not publish withdrawal or
// activate native cleanup. An orphan intent instead records an already absent
// publication: persisting it makes that intent available to retirement readers.
// Its producer must recheck authoritative owner absence and the full captured
// source before this call. Neither mode proves native completion or release.
func (w RemoteWithdrawalIntent) ReserveReceiveWithdrawal(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID, inventoryUID string, mesh *corev1.Secret, snapshot handover.Snapshot) error {
	if mesh == nil {
		return fmt.Errorf("withdrawal requires a mesh")
	}
	inventory, object, err := tunnel.ReadReceiveInventory(ctx, maps, clusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return err
	}
	if string(object.UID) != inventoryUID {
		return fmt.Errorf("withdrawal inventory incarnation changed")
	}
	if _, err := w.receiveRetirementPlan(snapshot, inventory, mesh, false); err != nil {
		return err
	}
	history, err := snapshot.Transition().Bytes()
	if err != nil {
		return err
	}
	record := receiveWithdrawalRecord{Version: 1, JournalSHA256: tunnel.HashPeerList(history), Withdrawal: w}
	if existing := inventory.Publications[w.MachineUID].Retirement; len(existing) != 0 {
		previous, err := decodeReceiveWithdrawalRecord(existing, w.MachineUID)
		if err != nil {
			return err
		}
		if previous.JournalSHA256 != record.JournalSHA256 || !reflect.DeepEqual(previous.Withdrawal, w) {
			return fmt.Errorf("persisted withdrawal differs from captured authority")
		}
		// Never upgrade or recapture an existing record after an ambiguous write.
		return tunnel.ReserveReceiveRetirement(ctx, maps, clusterUID, inventoryUID, mesh, snapshot, existing)
	}
	if w.SourceDataHash != "" && !w.Orphaned {
		record.Version = 2
		record.PeerData, err = captureWithdrawalPeerData(w, mesh.Data)
		if err != nil {
			return err
		}
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return tunnel.ReserveReceiveRetirement(ctx, maps, clusterUID, inventoryUID, mesh, snapshot, raw)
}

func captureWithdrawalPeerData(w RemoteWithdrawalIntent, data map[string][]byte) (map[string][]byte, error) {
	remaining, present, err := tunnel.WithdrawRemotePeer(data, w.MachineName, w.PublicKey)
	if err != nil || !present {
		return nil, fmt.Errorf("withdrawal capture requires its published peer fields")
	}
	fields := map[string][]byte{}
	for name, value := range data {
		if _, remains := remaining[name]; !remains {
			fields[name] = bytes.Clone(value)
		}
	}
	return fields, nil
}

// ReadReceiveWithdrawal admits only the persisted intent after publication of
// removal. Callers still verify live object revisions, ancestry and local native
// ownership. Returning an intent is not a receipt from any retained participant.
func ReadReceiveWithdrawal(inventory tunnel.ReceiveInventory, machineUID string, snapshot handover.Snapshot, mesh *corev1.Secret) (RemoteWithdrawalIntent, error) {
	return readReceiveWithdrawalStage(inventory, machineUID, snapshot, mesh, true)
}

// ReadPreparedReceiveWithdrawal recovers the already persisted consumer set
// before publication. It requires the original mesh revision and peer to still
// be present for legacy intents. Captures carrying a data hash also admit later
// metadata revisions with identical data. Returning this intent does not authorize
// native cleanup: callers
// must recheck live drain authority and publish through optimistic locking.
// Never recapture consumers after a lost response or controller restart.
func ReadPreparedReceiveWithdrawal(inventory tunnel.ReceiveInventory, machineUID string, snapshot handover.Snapshot, mesh *corev1.Secret) (RemoteWithdrawalIntent, error) {
	return readReceiveWithdrawalStage(inventory, machineUID, snapshot, mesh, false)
}

func readReceiveWithdrawalStage(inventory tunnel.ReceiveInventory, machineUID string, snapshot handover.Snapshot, mesh *corev1.Secret, withdrawn bool) (RemoteWithdrawalIntent, error) {
	fail := func(err error) (RemoteWithdrawalIntent, error) { return RemoteWithdrawalIntent{}, err }
	record, err := decodeReceiveWithdrawalRecord(inventory.Publications[machineUID].Retirement, machineUID)
	if err != nil {
		return fail(err)
	}
	history, err := snapshot.Transition().Bytes()
	if err != nil {
		return fail(err)
	}
	if record.Withdrawal.MachineUID != machineUID || record.JournalSHA256 != tunnel.HashPeerList(history) {
		return fail(fmt.Errorf("withdrawal record differs from fenced journal"))
	}
	if !withdrawn && record.Version == 2 {
		fields, err := captureWithdrawalPeerData(record.Withdrawal, mesh.Data)
		if err != nil || !reflect.DeepEqual(fields, record.PeerData) {
			return fail(fmt.Errorf("captured peer fields differ before withdrawal publication"))
		}
	}
	if _, err := record.Withdrawal.receiveRetirementPlan(snapshot, inventory, mesh, withdrawn); err != nil {
		return fail(err)
	}
	return record.Withdrawal, nil
}

// RecordedReceiveWithdrawal decodes persisted intent without granting execution
// authority. Offline recovery may use it only to exclude peers after matching
// the entire inventory to a previously verified native observation. Live callers
// must use ReadReceiveWithdrawal to validate publication and fenced history.
func RecordedReceiveWithdrawal(raw []byte, machineUID string) (RemoteWithdrawalIntent, error) {
	record, err := decodeReceiveWithdrawalRecord(raw, machineUID)
	return record.Withdrawal, err
}

func decodeReceiveWithdrawalRecord(raw []byte, machineUID string) (receiveWithdrawalRecord, error) {
	fail := func(err error) (receiveWithdrawalRecord, error) { return receiveWithdrawalRecord{}, err }
	var record receiveWithdrawalRecord
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&record); err != nil {
		return fail(err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return fail(fmt.Errorf("trailing withdrawal data"))
	}
	if (record.Version != 1 && record.Version != 2) || machineUID == "" || record.Withdrawal.MachineUID != machineUID || len(record.JournalSHA256) != 64 {
		return fail(fmt.Errorf("invalid recorded withdrawal identity"))
	}
	if record.Version == 1 && len(record.PeerData) != 0 {
		return fail(fmt.Errorf("legacy withdrawal cannot carry peer reconstruction fields"))
	}
	if record.Version == 2 {
		w := record.Withdrawal
		remaining, present, err := tunnel.WithdrawRemotePeer(record.PeerData, w.MachineName, w.PublicKey)
		if err != nil || !present || len(remaining) != 0 || w.Orphaned || w.SourceDataHash == "" {
			return fail(fmt.Errorf("withdrawal peer reconstruction exceeds its captured identity"))
		}
	}
	return record, nil
}
