package nodegroup

import (
	"fmt"
	"k8s.io/client-go/kubernetes"
	"time"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// Register enables experimental node groups for one mesh namespace. The current
// deployment manages CAPI and workload objects through the same cluster API.
func Register(mgr ctrl.Manager, namespace, mesh, apiVIP, apiPort, bootstrapFormat string, retirementAge ...time.Duration) error {
	if mgr == nil || namespace == "" || mesh == "" || apiPort == "" {
		return fmt.Errorf("manager, namespace, mesh and API port required")
	}
	direct, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return err
	}
	core, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return err
	}
	maxAge := time.Minute
	if len(retirementAge) > 0 {
		maxAge = retirementAge[0]
	}
	if maxAge <= 0 {
		return fmt.Errorf("positive retirement observation lifetime required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("node-group").
		For(&v1alpha1.ProvisionedNodeGroupClaim{}).
		Owns(&v1alpha1.ProvisionedNodeClaim{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool { return obj.GetNamespace() == namespace })).
		Complete(&Reconciler{RetirementAPI: core, RetirementMaxAge: maxAge, API: direct, Workload: direct, MachineGVK: schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}, MeshName: mesh, APIVIP: apiVIP, APIPort: apiPort, BootstrapSecretNameFormat: bootstrapFormat})
}
