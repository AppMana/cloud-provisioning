package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Install synthetic legacy completion/finalization into the real API fixture.
// Authentication is covered separately by attachment's real-token tests. Old
// receipt times and boot IDs prove that historical evidence is not reused as a
// fresh local observation. This fixture does not measure native or boot recovery.
func seedReceiveFinalization(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, corrupt bool) {
	t.Helper()
	ctx := context.Background()
	source, err := attachment.ReadReceiveRetirementSet(ctx, api, store, scope.Namespace, scope.MeshName, scope.MeshUID, scope.MachineUID)
	if err != nil {
		t.Fatal(err)
	}
	index, err := store.ConfigMaps.Get(ctx, source.Metadata.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	original := index.DeepCopy()
	t.Cleanup(func() {
		current, err := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
		if err != nil {
			t.Error(err)
			return
		}
		current.Data = original.Data
		if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Error(err)
		}
	})
	marshal := func(value any) json.RawMessage {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	p := source.Inventory.Publications[scope.MachineUID]
	binding := tunnel.HashPeerList(marshal(struct {
		Kind             string          `json:"kind"`
		ClusterUID       string          `json:"clusterUID"`
		InventoryUID     string          `json:"inventoryUID"`
		InventoryVersion string          `json:"inventoryVersion"`
		MeshUID          string          `json:"meshUID"`
		MeshVersion      string          `json:"meshVersion"`
		Withdrawal       json.RawMessage `json:"withdrawal"`
	}{"cldt/receive-retirement-observation/v1", store.ClusterUID, string(index.UID), "prior-completion-source", scope.MeshUID, source.MeshMetadata.ResourceVersion, p.Retirement}))
	at := time.Now().Add(-time.Hour)
	receipts := []handover.Receipt{}
	for _, c := range source.Withdrawal.Consumers {
		receipts = append(receipts, handover.Receipt{NodeUID: c.NodeUID, RoundHash: binding, ObservedAt: at.Add(-time.Second), ObservedBootID: "historical-boot", AgentPodName: "historical-agent", AgentPodUID: "historical-pod", AgentContainerName: "dialer", AgentContainerID: "historical-container"})
	}
	p.RetirementCompletion = marshal(map[string]any{"version": 2, "inventoryVersion": "prior-completion-source", "inventoryHash": "historical-inventory-hash", "meshVersion": source.MeshMetadata.ResourceVersion, "binding": binding, "receipts": receipts})
	view, err := source.Journals[scope.MachineUID].Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	p.RetirementFinalization = marshal(map[string]any{"version": 1, "machineUID": scope.MachineUID, "inventory": source.Metadata, "meshUID": scope.MeshUID, "meshVersion": source.MeshMetadata.ResourceVersion, "journalUID": string(source.Journals[scope.MachineUID].Metadata().UID), "finalizedAt": at, "maxReceiptAge": view.Round.MaxReceiptAge, "withdrawalSHA256": tunnel.HashPeerList(p.Retirement), "completionSHA256": tunnel.HashPeerList(p.RetirementCompletion)})
	if corrupt {
		p.RetirementFinalization = json.RawMessage(`{"version":99}`)
	}
	source.Inventory.Publications[scope.MachineUID] = p
	index.Data["inventory.json"] = string(marshal(source.Inventory))
	if _, err := store.ConfigMaps.Update(ctx, index, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}
