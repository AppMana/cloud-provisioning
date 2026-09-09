package nodegroup

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const GroupFinalizer = "cloud-provisioning.appmana.com/node-group"

// Reconciler orchestrates claims independently of their machine provider. API
// must bypass the cache so planning observes completed creation and deletion.
// This experimental reconciler is not registered in the production manager yet.
type Reconciler struct {
	API        client.Client
	Workload   client.Client
	MachineGVK schema.GroupVersionKind
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.API == nil {
		return ctrl.Result{}, fmt.Errorf("direct API client required")
	}
	group := &v1alpha1.ProvisionedNodeGroupClaim{}
	if err := r.API.Get(ctx, req.NamespacedName, group); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	again := ctrl.Result{RequeueAfter: time.Millisecond}
	if group.DeletionTimestamp.IsZero() && !slices.Contains(group.Finalizers, GroupFinalizer) {
		group.Finalizers = append(group.Finalizers, GroupFinalizer)
		return again, r.API.Update(ctx, group)
	}
	claims := &v1alpha1.ProvisionedNodeClaimList{}
	if err := r.API.List(ctx, claims, client.InNamespace(group.Namespace), client.MatchingLabels{GroupUIDLabel: string(group.UID)}); err != nil {
		return ctrl.Result{}, err
	}
	for i := range claims.Items {
		if _, err := ObserveChild(group, &claims.Items[i]); err != nil {
			return ctrl.Result{}, err
		}
	}
	if action := group.Status.PendingAction; action != nil {
		switch action.Type {
		case CreateAction:
			child, err := ResumeCreation(ctx, r.API, req.NamespacedName, group.UID, action.ID)
			if err != nil {
				return ctrl.Result{}, err
			}
			_, err = CompleteCreation(ctx, r.API, group, child.UID)
			return again, err
		case DrainAction:
			if r.Workload == nil {
				return ctrl.Result{}, fmt.Errorf("group removal awaits drain and attachment withdrawal integration")
			}
			bound, err := BindDrainNode(ctx, r.API, r.Workload, r.MachineGVK, group)
			if err != nil {
				return ctrl.Result{}, err
			}
			if action.NodeUID == "" {
				return again, nil
			}
			target := bound.Status.PendingAction
			empty, err := DrainNode(ctx, r.Workload, target.NodeName, types.UID(target.NodeUID))
			if err != nil {
				return ctrl.Result{}, err
			}
			if !empty {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			return ctrl.Result{}, fmt.Errorf("drained group worker awaits attachment withdrawal integration")
		default:
			return ctrl.Result{}, fmt.Errorf("unknown pending group action")
		}
	}
	if !group.DeletionTimestamp.IsZero() && len(claims.Items) == 0 {
		group.Finalizers = slices.DeleteFunc(group.Finalizers, func(v string) bool { return v == GroupFinalizer })
		return ctrl.Result{}, r.API.Update(ctx, group)
	}
	action, err := ProposeAction(group, claims.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	if action != nil {
		_, err = ReserveAction(ctx, r.API, group, action)
		return again, err
	}
	// Counts include provisioning and terminating claims. Readiness requires a
	// separately verified Node and attachment observation, not just claim creation.
	count := int32(len(claims.Items))
	if group.Status.Replicas != count {
		group.Status.Replicas = count
		return again, r.API.Status().Update(ctx, group)
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}
