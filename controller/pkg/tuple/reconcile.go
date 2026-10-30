package tuple

import (
	"context"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni/site"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReconcileReport summarizes changes applied (or planned) by ReconcileCalicoSite.
type ReconcileReport = site.ReconcileReport

// AlignLinuxCalicoMTU inspects a Linux calico-node DaemonSet and ensures its
// install-cni CNI_NETWORK_CONFIG "mtu" and FELIX_*MTU agree.
func AlignLinuxCalicoMTU(ds *appsv1.DaemonSet, targetMTU int) (bool, int, []string, error) {
	return site.AlignLinuxCalicoMTU(ds, targetMTU)
}

// AlignWindowsBGPConfigMap reconciles kube-system/calico-windows-config-actual
// so Windows BGP nodes have explicit IP="autodetect", IP6="autodetect" (when
// dual-stack), CALICO_DSR_DISABLE="true", and CNI_MTU / FELIX_MTU aligned with
// the cluster's effective Calico MTU.
func AlignWindowsBGPConfigMap(cm *corev1.ConfigMap, effectiveMTU int) (bool, []string) {
	return site.AlignWindowsBGPConfigMap(cm, effectiveMTU)
}

// AlignWindowsCalicoDaemonSetMTU aligns literal CNI_MTU and FELIX_*MTU env vars
// on kube-system/calico-node-windows when present.
func AlignWindowsCalicoDaemonSetMTU(ds *appsv1.DaemonSet, effectiveMTU int) (bool, []string) {
	return site.AlignWindowsCalicoDaemonSetMTU(ds, effectiveMTU)
}

// ReconcileCalicoSite ensures that the cluster's Linux calico-node DaemonSet,
// Windows BGP ConfigMap (calico-windows-config-actual), and Windows Calico
// DaemonSet (calico-node-windows) have aligned MTU and Windows BGP settings.
//
// It is shared between cloud-provisioning's runtime site.Excluder.Ensure and
// the standalone platform-tuple CLI (--mode=fix).
func ReconcileCalicoSite(ctx context.Context, c client.Client, r client.Reader, targetMTU int) (*ReconcileReport, error) {
	return site.ReconcileCalicoSite(ctx, c, r, targetMTU)
}
