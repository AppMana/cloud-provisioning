package attachment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"k8s.io/client-go/kubernetes"
)

// VerifyReceiveRetirementCompletion checks controller-authenticated durable
// evidence without storing or replaying bearer tokens. Versions three and four
// exclude completion evidence from the inventory authority hash; version two
// retains its own-record-only exclusion. Version four receipts bind authority
// independently of the completion's exact source-revision binding. Versions two
// and three retain revision-bound receipts. Current pod/container
// and boot identities and observation age remain mandatory. This is a deletion
// gate, not allocation release or evidence of future connectivity. Missing
// completion returns false; stale/incompatible evidence requires reconciliation.
func VerifyReceiveRetirementCompletion(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID, inventoryUID string, maxAge time.Duration) (bool, error) {
	_, verified, err := ReadVerifiedReceiveRetirementCompletion(ctx, api, store, namespace, meshName, meshUID, machineUID, inventoryUID, maxAge)
	return verified, err
}

// ReadVerifiedReceiveRetirementCompletion returns the exact admitted source
// whose durable completion passed verification. Missing or invalid evidence
// returns a zero source. Callers preparing a later write must retain this
// inventory UID/resourceVersion and the captured journal/mesh identities; a
// fresh read of a different revision cannot inherit this verification.
// This is not a release permit: finalization still needs live owner/consumer
// fencing, fresh observations and a conditional write against this source.
func ReadVerifiedReceiveRetirementCompletion(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID, inventoryUID string, maxAge time.Duration) (ReceiveRetirementSet, bool, error) {
	source, err := ReadReceiveRetirementSet(ctx, api, store, namespace, meshName, meshUID, machineUID)
	if err != nil {
		return ReceiveRetirementSet{}, false, err
	}
	if inventoryUID == "" || string(source.Metadata.UID) != inventoryUID || maxAge <= 0 {
		return ReceiveRetirementSet{}, false, fmt.Errorf("completion requires exact inventory and observation lifetime")
	}
	publication := source.Inventory.Publications[machineUID]
	if len(publication.RetirementCompletion) == 0 {
		return ReceiveRetirementSet{}, false, nil
	}
	var completion receiveRetirementCompletion
	decoder := json.NewDecoder(bytes.NewReader(publication.RetirementCompletion))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&completion); err != nil {
		return ReceiveRetirementSet{}, false, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return ReceiveRetirementSet{}, false, fmt.Errorf("trailing completion evidence")
	}
	if (completion.Version < 2 || completion.Version > receiveRetirementCohortCompletionVersion) || completion.InventoryVersion == "" || completion.InventoryVersion == source.Metadata.ResourceVersion || completion.MeshVersion != source.MeshMetadata.ResourceVersion {
		return ReceiveRetirementSet{}, false, fmt.Errorf("completion source revision is missing or changed")
	}
	inventoryHash, err := receiveRetirementInventoryHash(source.Inventory, machineUID, completion.Version)
	if err != nil {
		return ReceiveRetirementSet{}, false, err
	}
	if completion.InventoryHash != inventoryHash {
		return ReceiveRetirementSet{}, false, fmt.Errorf("inventory changed since completion authentication")
	}
	digest, err := receiveRetirementDigest(store.ClusterUID, inventoryUID, completion.InventoryVersion, source.Mesh, publication.Retirement)
	if err != nil {
		return ReceiveRetirementSet{}, false, err
	}
	if completion.Binding != digest {
		return ReceiveRetirementSet{}, false, fmt.Errorf("completion no longer binds withdrawal")
	}
	if completion.Version >= 4 {
		digest, err = receiveRetirementAuthorityDigest(store.ClusterUID, inventoryUID, inventoryHash, source.Metadata, source.Mesh, publication.Retirement)
		if err != nil {
			return ReceiveRetirementSet{}, false, err
		}
	}
	consumers := source.Withdrawal.Consumers
	if completion.Version == receiveRetirementCohortCompletionVersion {
		cohort, err := source.RetirementCohort()
		if err != nil {
			return ReceiveRetirementSet{}, false, err
		}
		if len(cohort.MachineUIDs) < 2 || !slices.Equal(cohort.MachineUIDs, completion.Cohort) {
			return ReceiveRetirementSet{}, false, fmt.Errorf("joint completion member set changed")
		}
		consumers = cohort.Consumers
		digest, err = receiveJointRetirementDigest(digest, completion.Cohort)
		if err != nil {
			return ReceiveRetirementSet{}, false, err
		}
	} else if len(completion.Cohort) != 0 {
		return ReceiveRetirementSet{}, false, fmt.Errorf("legacy completion cannot carry joint authority")
	}
	scope := ReceiveRetirementReceiptScope{digest: digest, nodes: map[string]string{}}
	for _, consumer := range consumers {
		scope.nodes[consumer.NodeUID] = consumer.NodeName
	}
	if len(completion.Receipts) != len(scope.nodes) {
		return ReceiveRetirementSet{}, false, fmt.Errorf("completion omits retained consumers")
	}
	seen := map[string]bool{}
	for _, receipt := range completion.Receipts {
		if seen[receipt.NodeUID] {
			return ReceiveRetirementSet{}, false, fmt.Errorf("duplicate completion consumer")
		}
		seen[receipt.NodeUID] = true
		if err := scope.validateObservation(receipt, time.Now(), maxAge); err != nil {
			return ReceiveRetirementSet{}, false, err
		}
		if err := handover.VerifyObservationIncarnation(ctx, api, namespace, scope.nodes[receipt.NodeUID], receipt); err != nil {
			return ReceiveRetirementSet{}, false, err
		}
	}
	if err := source.Recheck(ctx, api, store); err != nil {
		return ReceiveRetirementSet{}, false, err
	}
	for _, receipt := range completion.Receipts {
		if err := scope.validateObservation(receipt, time.Now(), maxAge); err != nil {
			return ReceiveRetirementSet{}, false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return ReceiveRetirementSet{}, false, err
	}
	return source, true, nil
}

// Completion records are observations of cleanup, not reservation or withdrawal
// authority. New records bind every inventory field except those observations.
// Legacy records retain their original interpretation; never silently upgrade
// their hash semantics. Copy publication entries so source admission is intact.
func receiveRetirementInventoryHash(inventory tunnel.ReceiveInventory, machineUID string, version int) (string, error) {
	if version < 2 || version > receiveRetirementCohortCompletionVersion {
		return "", fmt.Errorf("unsupported retirement completion version")
	}
	if _, found := inventory.Publications[machineUID]; machineUID == "" || !found {
		return "", fmt.Errorf("completion target is not published")
	}
	publications := make(map[string]tunnel.ReceiveJournalPublication, len(inventory.Publications))
	for uid, publication := range inventory.Publications {
		if uid == machineUID || version >= 3 {
			publication.RetirementCompletion = nil
		}
		publications[uid] = publication
	}
	inventory.Publications = publications
	raw, err := json.Marshal(inventory)
	if err != nil {
		return "", err
	}
	return tunnel.HashPeerList(raw), nil
}
