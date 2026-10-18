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
	"k8s.io/client-go/kubernetes"
)

// VerifyReceiveRetirementCompletion checks controller-authenticated durable
// evidence without storing or replaying bearer tokens. Only the completion's own
// inventory write may differ from its authenticated source. Current pod/container
// and boot identities and observation age remain mandatory. This is a deletion
// gate, not allocation release or evidence of future connectivity. Missing
// completion returns false; stale/incompatible evidence requires reconciliation.
func VerifyReceiveRetirementCompletion(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID, inventoryUID string, maxAge time.Duration) (bool, error) {
	source, err := ReadReceiveRetirementSet(ctx, api, store, namespace, meshName, meshUID, machineUID)
	if err != nil {
		return false, err
	}
	if inventoryUID == "" || string(source.Metadata.UID) != inventoryUID || maxAge <= 0 {
		return false, fmt.Errorf("completion requires exact inventory and observation lifetime")
	}
	publication := source.Inventory.Publications[machineUID]
	if len(publication.RetirementCompletion) == 0 {
		return false, nil
	}
	var completion receiveRetirementCompletion
	decoder := json.NewDecoder(bytes.NewReader(publication.RetirementCompletion))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&completion); err != nil {
		return false, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return false, fmt.Errorf("trailing completion evidence")
	}
	if completion.Version != 2 || completion.InventoryVersion == "" || completion.InventoryVersion == source.Metadata.ResourceVersion || completion.MeshVersion != source.MeshMetadata.ResourceVersion {
		return false, fmt.Errorf("completion source revision is missing or changed")
	}
	publication.RetirementCompletion = nil
	source.Inventory.Publications[machineUID] = publication
	raw, err := json.Marshal(source.Inventory)
	if err != nil {
		return false, err
	}
	if completion.InventoryHash != tunnel.HashPeerList(raw) {
		return false, fmt.Errorf("inventory changed since completion authentication")
	}
	digest, err := receiveRetirementDigest(store.ClusterUID, inventoryUID, completion.InventoryVersion, source.Mesh, publication.Retirement)
	if err != nil {
		return false, err
	}
	if completion.Binding != digest {
		return false, fmt.Errorf("completion no longer binds withdrawal")
	}
	scope := ReceiveRetirementReceiptScope{digest: digest, nodes: map[string]string{}}
	for _, consumer := range source.Withdrawal.Consumers {
		scope.nodes[consumer.NodeUID] = consumer.NodeName
	}
	if len(completion.Receipts) != len(scope.nodes) {
		return false, fmt.Errorf("completion omits retained consumers")
	}
	seen := map[string]bool{}
	for _, receipt := range completion.Receipts {
		if seen[receipt.NodeUID] {
			return false, fmt.Errorf("duplicate completion consumer")
		}
		seen[receipt.NodeUID] = true
		if err := scope.validateObservation(receipt, time.Now(), maxAge); err != nil {
			return false, err
		}
		if err := handover.VerifyObservationIncarnation(ctx, api, namespace, scope.nodes[receipt.NodeUID], receipt); err != nil {
			return false, err
		}
	}
	if err := source.Recheck(ctx, api, store); err != nil {
		return false, err
	}
	for _, receipt := range completion.Receipts {
		if err := scope.validateObservation(receipt, time.Now(), maxAge); err != nil {
			return false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, nil
}
