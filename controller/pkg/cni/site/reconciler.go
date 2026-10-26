package site

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reconciler restores the exclusion on any site network DaemonSet that lacks
// it, including one created or rewritten by its installer after startup.
type Reconciler struct{ *Excluder }

func (r Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ds := &appsv1.DaemonSet{}
	if err := r.Client.Get(ctx, req.NamespacedName, ds); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !r.IsTarget(ds) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.Ensure(ctx, ds)
}

// Register watches every DaemonSet for site network agents.
func Register(mgr ctrl.Manager, e *Excluder) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("site-cni-exclusion").
		For(&appsv1.DaemonSet{}).
		Complete(Reconciler{e})
}
