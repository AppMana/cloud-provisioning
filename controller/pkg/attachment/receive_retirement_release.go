package attachment

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"k8s.io/client-go/kubernetes"
)

// ReleaseFinalizedReceiveRetirement binds a release to verified historical
// completion and exact current source revisions. The caller's check must admit
// current owner absence, durable consumer fences, and compatible running and
// rollback versions for this exact source. Historical finalization alone is not
// that authority. No live controller may enable release before these recovery
// and compatibility paths are qualified.
//
// A conflict or lost write response is returned without retry. If the operation
// may have committed, use ReadReleasedReceiveJournal for fresh historical
// readback; an already released record must not be treated as active authority.
func ReleaseFinalizedReceiveRetirement(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID, inventoryUID string, maxAge time.Duration, check func(ReceiveRetirementSet) error) error {
	return releaseFinalizedReceiveRetirement(ctx, api, store, namespace, meshName, meshUID, machineUID, inventoryUID, maxAge, check, nil)
}

// finalCheck runs after source rechecks at both release boundaries. Receipt
// callers use it to reject observations that aged out during API reads.
func releaseFinalizedReceiveRetirement(ctx context.Context, api kubernetes.Interface, store handover.Store, namespace, meshName, meshUID, machineUID, inventoryUID string, maxAge time.Duration, check func(ReceiveRetirementSet) error, finalCheck func() error) error {
	if api == nil || store.ConfigMaps == nil || check == nil {
		return fmt.Errorf("release requires API, retained history and live release authority")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	source, verified, err := ReadReceiveRetirementFinalization(ctx, api, store, namespace, meshName, meshUID, machineUID, inventoryUID, maxAge)
	if err != nil {
		return err
	}
	if !verified {
		return fmt.Errorf("release requires verified retirement finalization")
	}
	// A joint proof depends on all retained member journals. Individual release
	// would discard authority needed by the other members; only an atomic cohort
	// release protocol may consume these completions.
	var completion receiveRetirementCompletion
	if err := json.Unmarshal(source.Inventory.Publications[machineUID].RetirementCompletion, &completion); err != nil {
		return err
	}
	if completion.Version == receiveRetirementCohortCompletionVersion {
		return fmt.Errorf("joint retirement requires atomic cohort release")
	}
	// Supply the authenticated source to the policy check, not a separately read
	// withdrawal that could name a different owner or captured consumer set.
	recheck := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := check(source); err != nil {
			return err
		}
		if err := source.Recheck(ctx, api, store); err != nil {
			return err
		}
		if finalCheck != nil {
			if err := finalCheck(); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	if err := recheck(); err != nil {
		return err
	}
	return tunnel.ReleaseReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, source.Metadata, source.Mesh, source.Journals[machineUID], source.Inventory.Publications[machineUID], recheck)
}
