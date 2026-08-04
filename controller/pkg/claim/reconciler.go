// Package claim expands a ProvisionedNodeClaim, the single resource a
// user commits, into the CAPI Machine + provider machine pair. The
// claim names no cloud: fulfillment routes through the registered
// join.MachineProvisioner whose ClusterGVK matches the CAPI Cluster's
// infrastructure, so "which cloud" is the existing provider
// abstraction's decision, exactly as it is for the join path.
//
// Everything downstream is other loops' business: CAPA (or its
// equivalent) launches the compute, join.Reconciler renders the
// tunnel-bootstrapping userdata into the Machine-owned bootstrap
// Secret, the endpoint-controller runs the mesh. This reconciler is a
// thin composition over CAPI: no bin-packing, no consolidation, no
// pod-watching. It is not an autoscaler.
package claim

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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

// infraTemplateVersion is the infrastructure API version this reads
// machine templates at. Providers move together with Cluster API's
// contract, so one version covers every provider on a given release.
const infraTemplateVersion = "v1beta2"

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

	// BootstrapSecretNameFormat mirrors join.Reconciler's. The Machine
	// is created with spec.bootstrap.dataSecretName already pointing at
	// the Secret the join reconciler will create.
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
		return r.reconcileDelete(ctx, claim)
	}
	// Own the teardown explicitly with a finalizer. OwnerRef garbage
	// collection is not sufficient: Cluster API's own Machine
	// controller reconciles ownerReferences and replaces them with the
	// Cluster, so the claim's reference is gone within seconds of
	// creation. Deleting the claim would then leave the Machine Running
	// with no deletionTimestamp and its instance still up. The claim
	// owns this lifecycle, so it does the deleting itself.
	if !containsString(claim.Finalizers, claimFinalizer) {
		updated := claim.DeepCopy()
		updated.Finalizers = append(updated.Finalizers, claimFinalizer)
		if err := r.Update(ctx, updated); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		claim = updated
	}

	cluster, err := r.resolveCluster(ctx, claim)
	if err != nil {
		return r.fail(ctx, claim, err)
	}
	if err := r.ensureClusterProvisioned(ctx, cluster); err != nil {
		return r.fail(ctx, claim, err)
	}
	infraKind, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "kind")
	provisioner := r.provisionerForClusterKind(infraKind)
	if provisioner == nil {
		return r.fail(ctx, claim, fmt.Errorf("no registered provider fulfills clusters of infrastructure kind %q", infraKind))
	}

	if _, err := nodeRequest(claim); err != nil {
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
	// objects are left untouched. CAPA specs are immutable, so
	// "change the claim" is delete-and-recreate, the same as any other
	// CAPI spec change.
	infraMachine := &unstructured.Unstructured{}
	infraMachine.SetGroupVersionKind(provisioner.GVK())
	err = r.Reader.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}, infraMachine)
	if apierrors.IsNotFound(err) {
		infraMachine, err = machineFromTemplate(ctx, r.Reader, claim)
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
		log.Info("created provider machine", "kind", infraMachine.GetKind(), "template", claim.Spec.InfrastructureRef.Name)
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking for provider machine: %w", err)
	}

	// Surfaced for the printer column when the provider has such a
	// concept; a provider without one simply reports nothing.
	instanceType, _, _ := unstructured.NestedString(infraMachine.Object, "spec", "instanceType")

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
				// Deletion has to converge. CAPI's defaults are to wait
				// indefinitely: drain, volume detach and node deletion
				// each block without a bound. The node these claims
				// create is reached only through the tunnel being torn
				// down, so it can become undrainable mid-deletion, and
				// `kubectl delete provisionednodeclaim` would then hang
				// with its instance still running. Bounded here so a
				// claim deletion is a reliable teardown.
				"deletion": map[string]any{
					"nodeDrainTimeoutSeconds":        int64(120),
					"nodeVolumeDetachTimeoutSeconds": int64(120),
					"nodeDeletionTimeoutSeconds":     int64(60),
				},
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
	// Keep converging until the Machine reaches a terminal phase.
	// Without this the claim reports whatever was true at the instant it
	// was created ("Resolving") forever, because nothing re-triggers
	// this reconciler when the Machine progresses underneath it.
	switch phase {
	case "Running", "Failed", "Deleting":
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
}

// claimFinalizer keeps the claim alive until the compute it created is
// gone, so `kubectl delete provisionednodeclaim` is a reliable
// teardown rather than an orphaning operation.
const claimFinalizer = "cloud-provisioning.appmana.com/claim"

// reconcileDelete removes what this claim created, in dependency
// order, and only then releases the claim. Deleting the CAPI Machine
// is what makes the infrastructure provider terminate the instance;
// the provider machine is deleted too in case it was created before
// the Machine (the window where nothing else would ever collect it).
func (r *Reconciler) reconcileDelete(ctx context.Context, claim *v1alpha1.ProvisionedNodeClaim) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	key := types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}

	remaining := 0
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	switch err := r.Reader.Get(ctx, key, machine); {
	case err == nil:
		remaining++
		if machine.GetDeletionTimestamp().IsZero() {
			if err := r.Delete(ctx, machine); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("deleting Machine %s: %w", key, err)
			}
			log.Info("deleting Machine for claim teardown", "machine", key)
		}
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, fmt.Errorf("checking Machine during teardown: %w", err)
	}

	for _, p := range r.Provisioners {
		infraMachine := &unstructured.Unstructured{}
		infraMachine.SetGroupVersionKind(p.GVK())
		switch err := r.Reader.Get(ctx, key, infraMachine); {
		case err == nil:
			remaining++
			if infraMachine.GetDeletionTimestamp().IsZero() {
				if err := r.Delete(ctx, infraMachine); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("deleting %s %s: %w", p.GVK().Kind, key, err)
				}
			}
		case !apierrors.IsNotFound(err) && !isMissingKind(err):
			return ctrl.Result{}, fmt.Errorf("checking %s during teardown: %w", p.GVK().Kind, err)
		}
	}

	if remaining > 0 {
		// Still terminating (the Machine drains its node first, with
		// the bounded timeouts set at creation). Hold the finalizer so
		// the claim visibly represents live infrastructure.
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// The Node outlives the machine it described. Kubernetes cannot
	// collect it on its own: a Node is cluster-scoped and this claim is
	// not, so it cannot be an owner, and Cluster API only removes nodes
	// for clusters it manages. Left alone it stays registered and
	// NotReady forever, so it is removed here.
	if err := r.deleteClaimedNodes(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}

	if containsString(claim.Finalizers, claimFinalizer) {
		updated := claim.DeepCopy()
		updated.Finalizers = removeString(updated.Finalizers, claimFinalizer)
		if err := r.Update(ctx, updated); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
		}
	}
	log.Info("claim teardown complete", "claim", key)
	return ctrl.Result{}, nil
}

// isMissingKind reports whether the error means the Kind isn't served
// at all (a provider whose CRDs aren't installed). Nothing of that
// kind can exist, so teardown has nothing to wait for.
func isMissingKind(err error) bool {
	return meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err)
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(list []string, s string) []string {
	out := list[:0]
	for _, item := range list {
		if item != s {
			out = append(out, item)
		}
	}
	return out
}

// ClaimAnnotation records which claim a Node was provisioned by. An
// annotation rather than an ownerReference: a cluster-scoped Node
// cannot be owned by a namespaced claim, and Kubernetes treats such an
// ownerReference as a reason to collect the object immediately.
const ClaimAnnotation = "cloud-provisioning.appmana.com/claim"

// deleteClaimedNodes removes the Nodes this claim produced.
func (r *Reconciler) deleteClaimedNodes(ctx context.Context, claim *v1alpha1.ProvisionedNodeClaim) error {
	nodes := &corev1.NodeList{}
	if err := r.Reader.List(ctx, nodes); err != nil {
		return fmt.Errorf("listing nodes during teardown: %w", err)
	}
	want := claim.Namespace + "/" + claim.Name
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Annotations[ClaimAnnotation] != want {
			continue
		}
		if err := r.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting node %s: %w", node.Name, err)
		}
		ctrl.LoggerFrom(ctx).Info("removed node for claim teardown", "node", node.Name)
	}
	return nil
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
		return nil, fmt.Errorf("%d CAPI Clusters in namespace %s: set spec.clusterName", len(list.Items), claim.Namespace)
	}
}

// ensureClusterProvisioned reports readiness for a Cluster annotated
// cluster.x-k8s.io/managed-by: external. That annotation tells Cluster
// API's own controllers to stand down, so nothing upstream sets the
// status, and until it is set Machines stay waiting for
// infrastructure. The external manager is this controller, so
// reporting it is this controller's job rather than a `kubectl patch`
// an operator has to know to run after every install.
func (r *Reconciler) ensureClusterProvisioned(ctx context.Context, cluster *unstructured.Unstructured) error {
	if cluster.GetAnnotations()["cluster.x-k8s.io/managed-by"] != "external" {
		return nil
	}
	infraKind, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "kind")
	infraGroup, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "apiGroup")
	infraName, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "name")
	if infraKind != "" && infraName != "" {
		infra := &unstructured.Unstructured{}
		infra.SetGroupVersionKind(schema.GroupVersionKind{Group: infraGroup, Version: clusterGVK.Version, Kind: infraKind})
		if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.GetNamespace(), Name: infraName}, infra); err != nil {
			return fmt.Errorf("getting %s %q: %w", infraKind, infraName, err)
		}
		if ready, _, _ := unstructured.NestedBool(infra.Object, "status", "ready"); !ready {
			patched := infra.DeepCopy()
			if err := unstructured.SetNestedField(patched.Object, true, "status", "ready"); err != nil {
				return err
			}
			if err := r.Status().Patch(ctx, patched, client.MergeFrom(infra)); err != nil {
				return fmt.Errorf("marking %s %q ready: %w", infraKind, infraName, err)
			}
		}
	}
	cpInit, _, _ := unstructured.NestedBool(cluster.Object, "status", "initialization", "controlPlaneInitialized")
	infraProv, _, _ := unstructured.NestedBool(cluster.Object, "status", "initialization", "infrastructureProvisioned")
	if cpInit && infraProv {
		return nil
	}
	patched := cluster.DeepCopy()
	for _, field := range []string{"controlPlaneInitialized", "infrastructureProvisioned"} {
		if err := unstructured.SetNestedField(patched.Object, true, "status", "initialization", field); err != nil {
			return err
		}
	}
	if err := r.Status().Patch(ctx, patched, client.MergeFrom(cluster)); err != nil {
		return fmt.Errorf("marking Cluster %q provisioned: %w", cluster.GetName(), err)
	}
	return nil
}

// clusterKubernetesVersion reads the running version off any existing
// Node: correct across upgrades without configuration, the same
// introspection the join providers already do.
func (r *Reconciler) clusterKubernetesVersion(ctx context.Context) (string, error) {
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes, client.Limit(1)); err != nil {
		return "", fmt.Errorf("listing nodes for version introspection: %w", err)
	}
	if len(nodes.Items) == 0 {
		return "", fmt.Errorf("no nodes found to introspect the kubernetes version from")
	}
	// Strip technology build suffixes ("v1.36.2+k0s"); Machine.spec
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

// machineFromTemplate builds the provider machine from the template
// the claim names. A Cluster API infrastructure machine template holds
// the machine's whole spec under spec.template.spec, and the machine is
// that spec carrying the provider's own kind.
func machineFromTemplate(ctx context.Context, reader client.Reader, claim *v1alpha1.ProvisionedNodeClaim) (*unstructured.Unstructured, error) {
	ref := claim.Spec.InfrastructureRef
	if ref.Kind == "" || ref.Name == "" {
		return nil, fmt.Errorf("spec.infrastructureRef needs a kind and a name")
	}
	if !strings.HasSuffix(ref.Kind, "Template") {
		return nil, fmt.Errorf("spec.infrastructureRef.kind %q is not a machine template", ref.Kind)
	}
	group := "infrastructure.cluster.x-k8s.io"
	if ref.APIGroup != nil && *ref.APIGroup != "" {
		group = *ref.APIGroup
	}
	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: infraTemplateVersion, Kind: ref.Kind})
	if err := reader.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: ref.Name}, template); err != nil {
		return nil, fmt.Errorf("getting %s %q: %w", ref.Kind, ref.Name, err)
	}
	// Every infrastructure machine template carries the machine's spec
	// here, and the machine it produces is the same kind without the
	// suffix. Both are Cluster API's provider contract rather than
	// anything specific to a cloud, which is why this needs no
	// knowledge of the provider it is talking to.
	spec, found, err := unstructured.NestedMap(template.Object, "spec", "template", "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("%s %q has no spec.template.spec", ref.Kind, ref.Name)
	}
	machine := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	machine.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   group,
		Version: infraTemplateVersion,
		Kind:    strings.TrimSuffix(ref.Kind, "Template"),
	})
	return machine, nil
}

func nodeRequest(claim *v1alpha1.ProvisionedNodeClaim) (join.NodeRequest, error) {
	req := join.NodeRequest{InternetFacing: true}
	if claim.Spec.InternetFacing != nil {
		req.InternetFacing = *claim.Spec.InternetFacing
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
