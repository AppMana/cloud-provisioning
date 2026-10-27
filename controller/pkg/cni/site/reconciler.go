package site

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// bgpRecheck is how often the site's BGP peering is checked again. Its
// resources exist only on Calico sites, so they are not watched.
const bgpRecheck = 5 * time.Minute

// Reconciler restores the exclusion on any site network DaemonSet that lacks
// it, including one created or rewritten by its installer after startup,
// and keeps the tunnel-node Calico agent in step with the site's.
type Reconciler struct{ *Excluder }

func (r Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ds := &appsv1.DaemonSet{}
	if err := r.Client.Get(ctx, req.NamespacedName, ds); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !r.IsTarget(ds) {
		return ctrl.Result{}, nil
	}
	if err := r.Ensure(ctx, ds); err != nil {
		return ctrl.Result{}, err
	}
	if isCalicoAgent(ds) {
		return ctrl.Result{RequeueAfter: bgpRecheck}, nil
	}
	return ctrl.Result{}, nil
}

// Register watches every DaemonSet for site network agents, and the
// tunnel agents, which bring their site DaemonSet back when changed.
func Register(mgr ctrl.Manager, e *Excluder) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("site-cni-exclusion").
		For(&appsv1.DaemonSet{}).
		Owns(&appsv1.DaemonSet{}).
		Complete(Reconciler{e})
}
