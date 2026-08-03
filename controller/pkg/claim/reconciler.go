// Package claim expands a ProvisionedNodeClaim -- the ONE resource a
// user commits -- into the CAPI Machine + provider machine pair. The
// claim names no cloud: fulfillment routes through the registered
// join.MachineProvisioner whose ClusterGVK matches the CAPI Cluster's
// infrastructure, so "which cloud" is the existing provider
// abstraction's decision, exactly as it is for the join path.
//
// Everything downstream is other loops' business: CAPA (or its
// equivalent) launches the compute, join.Reconciler renders the
// tunnel-bootstrapping userdata into the Machine-owned bootstrap
// Secret, the endpoint-controller runs the mesh. This reconciler is
// deliberately a thin composition over CAPI -- no bin-packing, no
// consolidation, no pod-watching. It is not an autoscaler and never
// will be.
package claim

import (
	"context"
	"fmt"
	"strings"

	v1alpha1 "github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	machineGVK     = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}
	machineListGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineList"}
	clusterGVK     = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}
	clusterListGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "ClusterList"}
)

const clusterNameLabel = "cluster.x-k8s.io/cluster-name"

// Reconciler expands claims.
type Reconciler struct {
	client.Client
	Reader client.Reader

	// Provisioners are the registered claim-fulfilling providers.
	Provisioners []join.MachineProvisioner

	// RoleLabel/RoleValue and TaintKey are the shared scheduling
	// contract (the endpoint-controller's Go constants, passed in so
	// there is exactly one definition).
	RoleLabel string
	RoleValue string

	// BootstrapSecretNameFormat mirrors join.Reconciler's -- the
	// Machine is created with spec.bootstrap.dataSecretName already
	// pointing at the Secret the join reconciler will create.
	BootstrapSecretNameFormat string

	// TunnelInterface is the mesh's interface name, surfaced in
	// status for operators running node-level asserts.
	TunnelInterface string
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	claim := &v1alpha1.ProvisionedNodeClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !claim.DeletionTimestamp.IsZero() {
		// Deletion is entirely ownerRef cascade: claim -> Machine +
		// provider machine -> (CAPA terminates the instance) and
		// Machine -> bootstrap Secret. Nothing to finalize here.
		return ctrl.Result{}, nil
	}

	cluster, err := r.resolveCluster(ctx, claim)
	if err != nil {
		return r.fail(ctx, claim, err)
	}
	infraKind, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "kind")
	provisioner := r.provisionerForClusterKind(infraKind)
	if provisioner == nil {
		return r.fail(ctx, claim, fmt.Errorf("no registered provider fulfills clusters of infrastructure kind %q", infraKind))
	}

	nodeReq, err := nodeRequest(claim)
	if err != nil {
		return r.fail(ctx, claim, err)
	}
	instanceType, err := provisioner.ResolveInstanceType(nodeReq)
	if err != nil {
		return r.fail(ctx, claim, err)
	}

	ownerRef := metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(),
		Kind:       "ProvisionedNodeClaim",
		Name:       claim.Name,
		UID:        claim.UID,
	}

	// Provider machine first (CAPA ignores it until the Machine
	// references it), then the Machine. Both idempotent: existing
	// objects are left untouched -- CAPA specs are immutable, and
	// "change the claim" is delete-and-recreate, same as every other
	// CAPI spec change.
	infraMachine := &unstructured.Unstructured{}
	infraMachine.SetGroupVersionKind(provisioner.GVK())
	err = r.Reader.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}, infraMachine)
	if apierrors.IsNotFound(err) {
		infraMachine, err = provisioner.InfraMachine(ctx, r.Reader, claim.Namespace, instanceType, nodeReq)
		if err != nil {
			return r.fail(ctx, claim, err)
		}
		infraMachine.SetName(claim.Name)
		infraMachine.SetNamespace(claim.Namespace)
		infraMachine.SetLabels(map[string]string{clusterNameLabel: cluster.GetName()})
		infraMachine.SetOwnerReferences([]metav1.OwnerReference{ownerRef})
		if err := r.Create(ctx, infraMachine); err != nil {
			return r.fail(ctx, claim, fmt.Errorf("creating %s: %w", provisioner.GVK().Kind, err))
		}
		log.Info("created provider machine", "kind", provisioner.GVK().Kind, "instanceType", instanceType)
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking for provider machine: %w", err)
	}

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	err = r.Reader.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}, machine)
	if apierrors.IsNotFound(err) {
		// spec.version from the live cluster: the joining worker must
		// match the control plane's version (kubeadm skew policy), and
		// some infra providers (CAPD) refuse a versionless Machine
		// outright.
		version, err := r.clusterKubernetesVersion(ctx)
		if err != nil {
			return r.fail(ctx, claim, err)
		}
		machine = &unstructured.Unstructured{Object: map[string]any{
			"spec": map[string]any{
				"clusterName": cluster.GetName(),
				"version":     version,
				"bootstrap": map[string]any{
					"dataSecretName": fmt.Sprintf(r.BootstrapSecretNameFormat, claim.Name),
				},
				"infrastructureRef": map[string]any{
					"apiGroup": provisioner.GVK().Group,
					"kind":     provisioner.GVK().Kind,
					"name":     claim.Name,
				},
			},
		}}
		machine.SetGroupVersionKind(machineGVK)
		machine.SetName(claim.Name)
		machine.SetNamespace(claim.Namespace)
		machine.SetLabels(map[string]string{
			r.RoleLabel:      r.RoleValue,
			clusterNameLabel: cluster.GetName(),
		})
		machine.SetOwnerReferences([]metav1.OwnerReference{ownerRef})
		if err := r.Create(ctx, machine); err != nil {
			return r.fail(ctx, claim, fmt.Errorf("creating Machine: %w", err))
		}
		log.Info("created Machine", "machine", claim.Name)
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking for Machine: %w", err)
	}

	// Status: mirror fulfillment progress off the Machine.
	phase, _, _ := unstructured.NestedString(machine.Object, "status", "phase")
	if phase == "" {
		phase = "Resolving"
	}
	externalIP := externalIPOf(machine)
	updated := claim.DeepCopy()
	updated.Status = v1alpha1.ProvisionedNodeClaimStatus{
		Phase:            phase,
		Provider:         provisioner.GVK().Kind,
		InstanceType:     instanceType,
		MachineName:      claim.Name,
		ExternalIP:       externalIP,
		WireGuardAddress: machine.GetAnnotations()["cloud-provisioning.appmana.com/wireguard-addr4"],
		TunnelInterface:  r.TunnelInterface,
	}
	if updated.Status != claim.Status {
		if err := r.Status().Update(ctx, updated); err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("updating claim status: %w", err)
		}
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) fail(ctx context.Context, claim *v1alpha1.ProvisionedNodeClaim, cause error) (ctrl.Result, error) {
	updated := claim.DeepCopy()
	updated.Status.Phase = "Failed"
	updated.Status.Message = cause.Error()
	if updated.Status != claim.Status {
		if err := r.Status().Update(ctx, updated); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("recording failure %q: %w", cause, err)
		}
	}
	return ctrl.Result{}, cause
}

// resolveCluster finds the CAPI Cluster the claim joins:
// spec.clusterName when set, otherwise the single Cluster in the
// claim's namespace (ambiguity is an error, not a guess).
func (r *Reconciler) resolveCluster(ctx context.Context, claim *v1alpha1.ProvisionedNodeClaim) (*unstructured.Unstructured, error) {
	if claim.Spec.ClusterName != "" {
		cluster := &unstructured.Unstructured{}
		cluster.SetGroupVersionKind(clusterGVK)
		if err := r.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: claim.Spec.ClusterName}, cluster); err != nil {
			return nil, fmt.Errorf("getting Cluster %q: %w", claim.Spec.ClusterName, err)
		}
		return cluster, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(clusterListGVK)
	if err := r.List(ctx, list, client.InNamespace(claim.Namespace)); err != nil {
		return nil, fmt.Errorf("listing Clusters: %w", err)
	}
	switch len(list.Items) {
	case 1:
		return &list.Items[0], nil
	case 0:
		return nil, fmt.Errorf("no CAPI Cluster in namespace %s (create one, or set spec.clusterName)", claim.Namespace)
	default:
		return nil, fmt.Errorf("%d CAPI Clusters in namespace %s -- set spec.clusterName", len(list.Items), claim.Namespace)
	}
}

// clusterKubernetesVersion reads the running version off any existing
// Node -- correct across upgrades without configuration, the same
// introspection the join providers already do.
func (r *Reconciler) clusterKubernetesVersion(ctx context.Context) (string, error) {
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes, client.Limit(1)); err != nil {
		return "", fmt.Errorf("listing nodes for version introspection: %w", err)
	}
	if len(nodes.Items) == 0 {
		return "", fmt.Errorf("no nodes found to introspect the kubernetes version from")
	}
	// Strip technology build suffixes ("v1.36.2+k0s") -- Machine.spec
	// .version is plain semver for every CAPI consumer.
	version := nodes.Items[0].Status.NodeInfo.KubeletVersion
	if i := strings.IndexByte(version, '+'); i > 0 {
		version = version[:i]
	}
	return version, nil
}

func (r *Reconciler) provisionerForClusterKind(kind string) join.MachineProvisioner {
	for _, p := range r.Provisioners {
		if p.ClusterGVK().Kind == kind {
			return p
		}
	}
	return nil
}

func nodeRequest(claim *v1alpha1.ProvisionedNodeClaim) (join.NodeRequest, error) {
	req := join.NodeRequest{Arch: claim.Spec.Arch, InternetFacing: true}
	if claim.Spec.InternetFacing != nil {
		req.InternetFacing = *claim.Spec.InternetFacing
	}
	if req.Arch == "" {
		req.Arch = "arm64"
	}
	if cpu, ok := claim.Spec.Requests[corev1.ResourceCPU]; ok {
		req.CPUMillis = cpu.MilliValue()
	}
	if mem, ok := claim.Spec.Requests[corev1.ResourceMemory]; ok {
		req.MemoryBytes = mem.Value()
	}
	if req.CPUMillis == 0 && req.MemoryBytes == 0 {
		return req, fmt.Errorf("spec.requests must set cpu and/or memory")
	}
	return req, nil
}

func externalIPOf(machine *unstructured.Unstructured) string {
	addresses, _, _ := unstructured.NestedSlice(machine.Object, "status", "addresses")
	for _, entry := range addresses {
		address, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if address["type"] != "ExternalIP" {
			continue
		}
		if ip, ok := address["address"].(string); ok && ip != "" {
			return ip
		}
	}
	return ""
}

// MachineListGVK is exported for the endpoint-controller's watch
// plumbing.
func MachineListGVK() schema.GroupVersionKind { return machineListGVK }
