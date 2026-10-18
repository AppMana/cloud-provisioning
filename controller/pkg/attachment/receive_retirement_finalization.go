package attachment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Finalization records consumption of authenticated evidence while all ownership
// remains reserved. It is not a current absence receipt or allocation release.
// Inventory metadata is captured, not rebased when other withdrawals progress.
type receiveRetirementFinalization struct {
	Version          int               `json:"version"`
	MachineUID       string            `json:"machineUID"`
	Inventory        metav1.ObjectMeta `json:"inventory"`
	MeshUID          string            `json:"meshUID"`
	MeshVersion      string            `json:"meshVersion"`
	JournalUID       string            `json:"journalUID"`
	FinalizedAt      time.Time         `json:"finalizedAt"`
	MaxReceiptAge    time.Duration     `json:"maxReceiptAge"`
	WithdrawalSHA256 string            `json:"withdrawalSHA256"`
	CompletionSHA256 string            `json:"completionSHA256"`
}

// FinalizeReceiveRetirementCompletion consumes fresh durable completion with
// exact source CAS. recheck must enforce the controller's live owner absence and
// consumer policy; it runs again at the write boundary. No reservation is freed.
// Existing finalization must be read and verified separately, never overwritten
// or silently retried using an old observation batch after an ambiguous response.
func FinalizeReceiveRetirementCompletion(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID, inventoryUID string, maxAge time.Duration, recheck func() error) error {
	if recheck == nil {
		return fmt.Errorf("finalization requires live ownership and consumer policy checks")
	}
	if err := recheck(); err != nil {
		return err
	}
	source, verified, err := ReadVerifiedReceiveRetirementCompletion(ctx, api, store, namespace, meshName, meshUID, machineUID, inventoryUID, maxAge)
	if err != nil {
		return err
	}
	if !verified {
		return fmt.Errorf("finalization requires verified completion")
	}
	publication := source.Inventory.Publications[machineUID]
	if len(publication.RetirementFinalization) != 0 {
		return fmt.Errorf("retirement already finalized; verify retained evidence")
	}
	var completion receiveRetirementCompletion
	if err := json.Unmarshal(publication.RetirementCompletion, &completion); err != nil {
		return err
	}
	snapshot := source.Journals[machineUID]
	record := receiveRetirementFinalization{Version: 1, MachineUID: machineUID, Inventory: *source.Metadata.DeepCopy(), MeshUID: meshUID, MeshVersion: source.MeshMetadata.ResourceVersion, JournalUID: string(snapshot.Metadata().UID), FinalizedAt: time.Now(), MaxReceiptAge: maxAge, WithdrawalSHA256: tunnel.HashPeerList(publication.Retirement), CompletionSHA256: tunnel.HashPeerList(publication.RetirementCompletion)}
	// ManagedFields are server bookkeeping and not part of receipt authority.
	record.Inventory.ManagedFields = nil
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	scope := ReceiveRetirementReceiptScope{nodes: map[string]string{}}
	for _, c := range source.Withdrawal.Consumers {
		scope.nodes[c.NodeUID] = c.NodeName
	}
	if len(completion.Receipts) == 0 {
		return fmt.Errorf("finalization has no authenticated consumers")
	}
	scope.digest = completion.Receipts[0].RoundHash
	check := func() error {
		if err := recheck(); err != nil {
			return err
		}
		if err := source.Recheck(ctx, api, store); err != nil {
			return err
		}
		for _, receipt := range completion.Receipts {
			if err := handover.VerifyObservationIncarnation(ctx, api, namespace, scope.nodes[receipt.NodeUID], receipt); err != nil {
				return err
			}
		}
		if err := source.Recheck(ctx, api, store); err != nil {
			return err
		}
		now := time.Now()
		for _, receipt := range completion.Receipts {
			if err := scope.validateObservation(receipt, now, maxAge); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	return tunnel.RecordReceiveRetirementFinalization(ctx, store.ConfigMaps, store.ClusterUID, source.Metadata, source.Mesh, snapshot, publication.Retirement, publication.RetirementCompletion, raw, check)
}

// ReadReceiveRetirementFinalization verifies a historical controller-owned
// finalization against retained withdrawal and journal identity. Observation age
// is checked at finalization, not extended to the present. This does NOT prove
// current native absence, current agent incarnation, reboot recovery, or permit
// reservation release. Agents must additionally admit their durable local fence;
// release needs its own live ownership protocol. Missing records return false.
func ReadReceiveRetirementFinalization(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID, inventoryUID string, maxAge time.Duration) (ReceiveRetirementSet, bool, error) {
	source, err := ReadReceiveRetirementSet(ctx, api, store, namespace, meshName, meshUID, machineUID)
	if err != nil {
		return ReceiveRetirementSet{}, false, err
	}
	ok, err := validateReceiveRetirementFinalization(source, store.ClusterUID, machineUID, inventoryUID, maxAge, time.Now())
	if err != nil || !ok {
		return ReceiveRetirementSet{}, false, err
	}
	if err := source.Recheck(ctx, api, store); err != nil {
		return ReceiveRetirementSet{}, false, err
	}
	return source, true, nil
}

func decodeRetirementEvidence(raw []byte, destination any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(destination); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return fmt.Errorf("trailing retirement evidence")
	}
	return nil
}

func validateReceiveRetirementFinalization(source ReceiveRetirementSet, clusterUID, machineUID, inventoryUID string, maxAge time.Duration, now time.Time) (bool, error) {
	fail := func(message string) (bool, error) { return false, fmt.Errorf("retirement finalization: %s", message) }
	publication := source.Inventory.Publications[machineUID]
	if len(publication.RetirementFinalization) == 0 {
		return false, nil
	}
	var record receiveRetirementFinalization
	if err := decodeRetirementEvidence(publication.RetirementFinalization, &record); err != nil {
		return false, err
	}
	snapshot, found := source.Journals[machineUID]
	if !found || record.Version != 1 || record.MachineUID != machineUID || inventoryUID == "" || string(source.Metadata.UID) != inventoryUID || record.Inventory.UID != source.Metadata.UID || record.Inventory.Name != source.Metadata.Name || record.Inventory.Namespace != source.Metadata.Namespace || record.Inventory.ResourceVersion == "" || record.Inventory.ResourceVersion == source.Metadata.ResourceVersion || record.Inventory.DeletionTimestamp != nil || record.MeshUID != string(source.MeshMetadata.UID) || record.MeshVersion == "" || record.JournalUID != string(snapshot.Metadata().UID) || !snapshot.Frozen() {
		return fail("source identity changed")
	}
	if maxAge <= 0 || record.MaxReceiptAge != maxAge || record.FinalizedAt.IsZero() || record.FinalizedAt.After(now) {
		return fail("invalid finalization time or observation policy")
	}
	if record.WithdrawalSHA256 != tunnel.HashPeerList(publication.Retirement) || record.CompletionSHA256 != tunnel.HashPeerList(publication.RetirementCompletion) {
		return fail("consumed evidence changed")
	}
	var completion receiveRetirementCompletion
	if err := decodeRetirementEvidence(publication.RetirementCompletion, &completion); err != nil {
		return false, err
	}
	if completion.Version < 2 || completion.Version > receiveRetirementCompletionVersion || completion.InventoryVersion == "" || completion.InventoryVersion == record.Inventory.ResourceVersion || completion.MeshVersion != record.MeshVersion {
		return fail("completion source changed")
	}
	historicalMesh := source.Mesh.DeepCopy()
	historicalMesh.ResourceVersion = record.MeshVersion
	digest, err := receiveRetirementDigest(clusterUID, inventoryUID, completion.InventoryVersion, historicalMesh, publication.Retirement)
	if err != nil {
		return false, err
	}
	if completion.Binding != digest {
		return fail("completion binding changed")
	}
	if completion.Version >= 4 {
		digest, err = receiveRetirementAuthorityDigest(clusterUID, inventoryUID, completion.InventoryHash, record.Inventory, historicalMesh, publication.Retirement)
		if err != nil {
			return false, err
		}
	}
	scope := ReceiveRetirementReceiptScope{digest: digest, nodes: map[string]string{}}
	for _, c := range source.Withdrawal.Consumers {
		scope.nodes[c.NodeUID] = c.NodeName
	}
	if len(completion.Receipts) != len(scope.nodes) || len(scope.nodes) == 0 {
		return fail("incomplete consumers")
	}
	seen := map[string]bool{}
	for _, receipt := range completion.Receipts {
		if seen[receipt.NodeUID] || receipt.ObservedBootID == "" || receipt.AgentPodName == "" || receipt.AgentPodUID == "" || receipt.AgentContainerName == "" || receipt.AgentContainerID == "" {
			return fail("missing or duplicate authenticated identity")
		}
		seen[receipt.NodeUID] = true
		if err := scope.validateObservation(receipt, record.FinalizedAt, maxAge); err != nil {
			return false, err
		}
	}
	return true, nil
}
