package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *meshReconciler) finishRetiredReceiveAdoptions(ctx context.Context, result ctrl.Result) (ctrl.Result, error) {
	progressed, err := r.reconcileRetiredReceiveAdoptions(ctx)
	return retiredReceiveAdoptionResult(ctx, result, progressed, err)
}

func retiredReceiveAdoptionResult(ctx context.Context, result ctrl.Result, progressed bool, err error) (ctrl.Result, error) {
	retry := time.Duration(0)
	if errors.Is(err, attachment.ErrRetirementObservationExpired) && ctx.Err() == nil {
		// Match pending claim retirement: expired evidence forbids mutation,
		// but waiting for its refresh must not enter exponential error backoff.
		retry = 5 * time.Second
	} else if err != nil {
		// controller-runtime ignores a requeue result when an error is returned.
		return ctrl.Result{}, fmt.Errorf("retiring captured adoption: %w", err)
	} else if progressed {
		retry = time.Second
	}
	if retry > 0 && (result.RequeueAfter == 0 || retry < result.RequeueAfter) {
		result.RequeueAfter = retry
	}
	return result, nil
}

// Called after DaemonSet reconciliation, so pending cleanup cannot block an
// agent update. Each pass makes at most one conditional Secret deletion.
func (r *meshReconciler) reconcileRetiredReceiveAdoptions(ctx context.Context) (bool, error) {
	if r.receivePreparation == nil || r.receivePreparation.Journal == nil || r.receivePreparation.Journal.Receipts == nil || !r.receivePreparation.Journal.Receipts.FinalizeRetirements {
		return false, nil
	}
	mesh := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
		return false, err
	}
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, r.receiveJournal.ConfigMaps, r.receiveJournal.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, uid := range inventory.MachineUIDs {
		pub := inventory.Publications[uid]
		if len(pub.Retirement) == 0 || len(pub.RetirementCompletion) == 0 {
			continue
		}
		withdrawal, err := attachment.RecordedReceiveWithdrawal(pub.Retirement, uid)
		if err != nil {
			return false, err
		}
		if withdrawal.Orphaned {
			continue
		}
		store := r.receiveJournal
		store.Name, store.ExpectedUID = tunnel.ReceiveTransitionName(uid), pub.JournalUID
		snapshot, err := store.Load(ctx)
		if err != nil {
			return false, err
		}
		plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
		if err != nil {
			return false, err
		}
		if pub.JournalUID == "" || len(pub.Plan) != 0 || plan.MachineUID != uid || plan.Namespace != mesh.Namespace || plan.MeshName != mesh.Name || plan.MeshUID != string(mesh.UID) {
			return false, fmt.Errorf("retired adoption journal scope changed")
		}
		if progress, err := r.reconcileRetiredReceiveAdoption(ctx, mesh, plan, snapshot, string(index.UID)); err != nil || progress {
			return progress, err
		}
	}
	return false, nil
}

// Normal withdrawal removes peer publication before deleting its lifecycle
// objects. Ordinary peer pruning therefore cannot discover the adoption Secret,
// and the retained reservation deliberately excludes it from orphan pruning.
// Retire only the captured Secret after verified cleanup and owner absence.
// This permits subsequent finalization; it does not release any reservation.
func (r *meshReconciler) reconcileRetiredReceiveAdoption(ctx context.Context, mesh *corev1.Secret, plan tunnel.ReceiveTransitionPlan, snapshot handover.Snapshot, inventoryUID string) (bool, error) {
	if r.receivePreparation == nil || r.receivePreparation.Journal == nil || r.receivePreparation.Journal.Receipts == nil || !r.receivePreparation.Journal.Receipts.FinalizeRetirements {
		return false, nil
	}
	adoption := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: plan.Namespace, Name: plan.AdoptionName}, adoption); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if string(adoption.UID) != plan.AdoptionUID || adoption.ResourceVersion == "" {
		return false, fmt.Errorf("retired adoption identity differs from reserved journal")
	}
	if adoption.DeletionTimestamp != nil {
		return false, nil
	}
	if r.receiveAPI == nil {
		return false, fmt.Errorf("retired adoption cleanup requires receipt verification API")
	}
	absent := func() (bool, error) {
		machine := &unstructured.Unstructured{}
		machine.SetGroupVersionKind(machineGVK)
		for _, owner := range []struct {
			key    client.ObjectKey
			object client.Object
		}{
			{client.ObjectKey{Namespace: plan.Namespace, Name: plan.MachineName}, machine},
			{client.ObjectKey{Namespace: plan.Namespace, Name: plan.MachineName}, &v1alpha1.ProvisionedNodeClaim{}},
			{client.ObjectKey{Name: plan.CloudNodeName}, &corev1.Node{}},
		} {
			err := r.reader.Get(ctx, owner.key, owner.object)
			if err == nil {
				return false, nil
			}
			if !apierrors.IsNotFound(err) {
				return false, err
			}
		}
		return true, ctx.Err()
	}
	if ok, err := absent(); err != nil || !ok {
		return false, err
	}
	store := r.receiveJournal
	store.Name, store.ExpectedUID = tunnel.ReceiveTransitionName(plan.MachineUID), string(snapshot.Metadata().UID)
	age, err := attachment.ReceiveRetirementReceiptAge(snapshot, r.receivePreparation.Journal.MaxReceiptAge.Duration)
	if err != nil {
		return false, err
	}
	source, verified, err := attachment.ReadVerifiedReceiveRetirementCompletion(ctx, r.receiveAPI, store, plan.Namespace, plan.MeshName, plan.MeshUID, plan.MachineUID, inventoryUID, age)
	if err != nil || !verified {
		return false, err
	}
	if source.Withdrawal.Orphaned || source.MeshMetadata.UID != mesh.UID || source.MeshMetadata.ResourceVersion != mesh.ResourceVersion {
		return false, fmt.Errorf("adoption retirement source changed or is not normal withdrawal")
	}
	if err := source.Journals[plan.MachineUID].ContinuesExecution(snapshot); err != nil {
		return false, err
	}
	if ok, err := absent(); err != nil || !ok {
		return false, err
	}
	if err := source.Recheck(ctx, r.receiveAPI, store); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// A same-name replacement or acknowledgement update must conflict. Never
	// retry deletion using a newly read Secret or bypass its finalizers.
	return r.deleteCapturedReceiveAdoption(ctx, adoption)
}

func (r *meshReconciler) deleteCapturedReceiveAdoption(ctx context.Context, adoption *corev1.Secret) (bool, error) {
	if adoption == nil || adoption.UID == "" || adoption.ResourceVersion == "" {
		return false, fmt.Errorf("adoption deletion requires captured identity and revision")
	}
	err := r.Delete(ctx, adoption, client.Preconditions{UID: &adoption.UID, ResourceVersion: &adoption.ResourceVersion})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}
