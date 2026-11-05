package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReceiveCoordinator isolates the optional make-before-break tunnel endpoint
// handover state machine (receivePreparation / handover.Store / mTLS receipt
// collection) behind a single interface seam so ordinary mesh, adoption, and
// DaemonSet reconciliation remain decoupled from multi-round handover internals.
type ReceiveCoordinator interface {
	// Active reports whether receiver preparation is enabled on this controller.
	Active() bool
	// ConfigureDaemonSet applies receiver sysctl and mTLS receipt flags/volumes
	// to a site or cloud dialer DaemonSet.
	ConfigureDaemonSet(ds *appsv1.DaemonSet)
	// ReconcileOrphanWithdrawals advances journaled withdrawals for deleted machines.
	ReconcileOrphanWithdrawals(ctx context.Context, secret *corev1.Secret) (bool, error)
	// FinishRetiredAdoptions completes post-reconcile retired adoption cleanup.
	FinishRetiredAdoptions(ctx context.Context, result ctrl.Result) (ctrl.Result, error)
	// ReconcileAdoption handles adoption Secret updates when receiver preparation
	// is active. When handled is true, the caller returns err directly.
	ReconcileAdoption(ctx context.Context, machine *unstructured.Unstructured, existing *corev1.Secret, doc []byte) (handled bool, err error)
	// RecoverPublishedTransition recovers expired published transitions when
	// normal CNI address reconciliation fails.
	RecoverPublishedTransition(ctx context.Context, machine *unstructured.Unstructured, now time.Time) error
	// JournalRoleResources returns any ConfigMap journal names and Pod get permissions
	// required by the dialer Role when journaled handover is enabled.
	JournalRoleResources(ctx context.Context, machines []unstructured.Unstructured) (journals []string, needPodsGet bool, err error)
}

func (r *meshReconciler) receiveCoordinator() ReceiveCoordinator {
	if r.receivePreparation == nil {
		return noopReceiveCoordinator{}
	}
	return &meshReceiveCoordinator{r: r}
}

type noopReceiveCoordinator struct{}

func (noopReceiveCoordinator) Active() bool                        { return false }
func (noopReceiveCoordinator) ConfigureDaemonSet(*appsv1.DaemonSet) {}
func (noopReceiveCoordinator) ReconcileOrphanWithdrawals(context.Context, *corev1.Secret) (bool, error) {
	return false, nil
}
func (noopReceiveCoordinator) FinishRetiredAdoptions(_ context.Context, result ctrl.Result) (ctrl.Result, error) {
	return result, nil
}
func (noopReceiveCoordinator) ReconcileAdoption(context.Context, *unstructured.Unstructured, *corev1.Secret, []byte) (bool, error) {
	return false, nil
}
func (noopReceiveCoordinator) RecoverPublishedTransition(context.Context, *unstructured.Unstructured, time.Time) error {
	return nil
}
func (noopReceiveCoordinator) JournalRoleResources(context.Context, []unstructured.Unstructured) ([]string, bool, error) {
	return nil, false, nil
}

type meshReceiveCoordinator struct {
	r *meshReconciler
}

func (c *meshReceiveCoordinator) Active() bool {
	return c.r.receivePreparation != nil
}

func (c *meshReceiveCoordinator) ConfigureDaemonSet(ds *appsv1.DaemonSet) {
	configureReceiveSysctlPolicy(ds)
	c.r.configureReceiptAgent(ds)
}

func (c *meshReceiveCoordinator) ReconcileOrphanWithdrawals(ctx context.Context, secret *corev1.Secret) (bool, error) {
	return c.r.reconcileOrphanReceiveWithdrawals(ctx, secret)
}

func (c *meshReceiveCoordinator) FinishRetiredAdoptions(ctx context.Context, result ctrl.Result) (ctrl.Result, error) {
	return c.r.finishRetiredReceiveAdoptions(ctx, result)
}

func (c *meshReceiveCoordinator) ReconcileAdoption(ctx context.Context, machine *unstructured.Unstructured, existing *corev1.Secret, doc []byte) (bool, error) {
	if !machine.GetDeletionTimestamp().IsZero() || machine.GetAnnotations()[attachment.DrainIntentAnnotation] != "" {
		return true, nil
	}
	_, preparing := existing.Data[tunnel.ReceivePrepareKey]
	if !preparing && remoteNeighborSourcesWithdrawn(existing.Data[tunnel.CloudPeersKey], doc) {
		hasJournal, err := c.r.hasReceiveJournal(ctx, machine)
		if err != nil {
			return true, err
		}
		if !hasJournal {
			patch := client.MergeFromWithOptions(existing.DeepCopy(), client.MergeFromWithOptimisticLock{})
			if existing.Data == nil {
				existing.Data = map[string][]byte{}
			}
			existing.Data[tunnel.CloudPeersKey] = doc
			return true, c.r.Patch(ctx, existing, patch)
		}
	}
	return true, c.r.reconcileReceivePreparation(ctx, machine)
}

func (c *meshReceiveCoordinator) RecoverPublishedTransition(ctx context.Context, machine *unstructured.Unstructured, now time.Time) error {
	return c.r.recoverPublishedReceiveTransition(ctx, machine, now)
}

func (c *meshReceiveCoordinator) JournalRoleResources(ctx context.Context, machines []unstructured.Unstructured) ([]string, bool, error) {
	r := c.r
	if r.receivePreparation == nil || r.receivePreparation.Journal == nil {
		return nil, false, nil
	}
	journals := make([]string, 0, len(machines))
	for i := range machines {
		if machines[i].GetNamespace() == r.secretNamespace {
			if machines[i].GetUID() == "" {
				return nil, false, fmt.Errorf("journal read access requires a machine UID")
			}
			journals = append(journals, tunnel.ReceiveTransitionName(string(machines[i].GetUID())))
		}
	}
	mesh := &corev1.Secret{}
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
		return nil, false, err
	}
	if mesh.UID == "" || mesh.DeletionTimestamp != nil {
		return nil, false, fmt.Errorf("journal read access requires a live mesh identity")
	}
	inventory, _, err := tunnel.ReadReceiveInventory(ctx, r.receiveJournal.ConfigMaps, r.receiveJournal.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, false, err
	}
	journals = append(journals, tunnel.ReceiveInventoryName(string(mesh.UID)))
	retained := append([]string{}, inventory.MachineUIDs...)
	for uid := range inventory.Released {
		retained = append(retained, uid)
	}
	sort.Strings(retained)
	for _, uid := range retained {
		name := tunnel.ReceiveTransitionName(uid)
		publication, released := inventory.Released[uid]
		if !released {
			publication = inventory.Publications[uid]
		}
		if publication.JournalUID != "" {
			store := r.receiveJournal
			store.Name, store.ExpectedUID = name, publication.JournalUID
			snapshot, err := store.Load(ctx)
			if err != nil {
				return nil, false, err
			}
			archives, err := store.ArchiveNames(ctx, snapshot)
			if err != nil {
				return nil, false, err
			}
			journals = append(journals, archives...)
			if !released {
				pending, err := r.receivePreparationArchive(ctx, store, snapshot)
				if err != nil {
					return nil, false, err
				}
				if pending != "" {
					journals = append(journals, pending)
				}
			}
		}
		found := false
		for _, current := range journals {
			found = found || current == name
		}
		if !found {
			journals = append(journals, name)
		}
	}
	sort.Strings(journals)
	needPodsGet := r.receivePreparation.Journal.Receipts != nil && r.receivePreparation.Journal.Receipts.Endpoint != ""
	return journals, needPodsGet, nil
}
