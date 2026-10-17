package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *meshReconciler) configureReceiveJournal(ctx context.Context, api kubernetes.Interface) error {
	namespace, err := api.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if namespace.UID == "" || namespace.DeletionTimestamp != nil {
		return fmt.Errorf("receiver journal requires a live cluster identity")
	}
	r.receiveJournal = handover.Store{ConfigMaps: api.CoreV1().ConfigMaps(r.secretNamespace), ClusterUID: string(namespace.UID)}
	return nil
}

func (r *meshReconciler) reconcileReceivePreparation(ctx context.Context, machine *unstructured.Unstructured) error {
	p := r.receivePreparation
	if p.Journal == nil {
		_, err := r.prepareRemoteReceivePaths(ctx, machine, p.NativeCIDRs, p.Ports)
		return err
	}
	policy, err := p.Journal.policy()
	if err != nil {
		return err
	}
	store := r.receiveJournal
	store.Name = tunnel.ReceiveTransitionName(string(machine.GetUID()))
	_, err = r.ensureReceiveTransition(ctx, machine, store, p.NativeCIDRs, p.Ports, policy, time.Now())
	return err
}

// ensureReceiveTransition resumes committed state before considering a new
// proposal. It neither advances phases nor rewrites an unfinished journal.
// Callers must authenticate fresh phase receipts and apply native actions only
// from committed rounds. A completed journal must be archived before reuse.
func (r *meshReconciler) ensureReceiveTransition(ctx context.Context, machine *unstructured.Unstructured, store handover.Store, native []netip.Prefix, ports []int, policy handover.Policy, now time.Time) (*handover.Snapshot, error) {
	if machine == nil || machine.GetUID() == "" || machine.GetNamespace() != r.secretNamespace || store.Name != tunnel.ReceiveTransitionName(string(machine.GetUID())) {
		return nil, fmt.Errorf("receiver journal requires a scoped machine identity")
	}
	mesh := &corev1.Secret{}
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
		return nil, err
	}
	currentMachine := &unstructured.Unstructured{}
	currentMachine.SetGroupVersionKind(machine.GetObjectKind().GroupVersionKind())
	if err := r.reader.Get(ctx, client.ObjectKeyFromObject(machine), currentMachine); err != nil {
		return nil, err
	}
	if currentMachine.GetUID() != machine.GetUID() {
		return nil, fmt.Errorf("receiver inventory machine identity changed")
	}
	if err := tunnel.ReserveReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, string(machine.GetUID()), r.owners()); err != nil {
		return nil, err
	}
	existing, err := store.Load(ctx)
	if err == nil {
		plan, err := tunnel.DecodeReceiveTransition(existing, store.ClusterUID)
		if err != nil {
			return nil, err
		}
		if plan.MachineUID != string(machine.GetUID()) || plan.MachineName != machine.GetName() || plan.Namespace != r.secretNamespace {
			return nil, fmt.Errorf("receiver journal belongs to another machine")
		}
		return &existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	prepared, err := r.prepareRemoteReceivePaths(ctx, machine, native, ports)
	if err != nil || prepared == nil {
		return nil, err
	}
	plan, err := r.buildReceiveTransitionPlan(ctx, machine, prepared, now)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	spec, err := tunnel.ReceiveTransitionSpec(plan, store.ClusterUID, hash)
	if err != nil {
		return nil, err
	}
	transition, err := handover.NewTransition(spec, now, policy)
	if err != nil {
		return nil, err
	}
	store.OwnerReferences = r.owners()
	persisted, err := store.CreateWithPlan(ctx, transition, raw)
	if err != nil {
		return nil, err
	}
	return &persisted, nil
}

func (r *meshReconciler) buildReceiveTransitionPlan(ctx context.Context, machine *unstructured.Unstructured, prepared *preparedRemoteReceivers, now time.Time) (tunnel.ReceiveTransitionPlan, error) {
	fail := func(err error) (tunnel.ReceiveTransitionPlan, error) { return tunnel.ReceiveTransitionPlan{}, err }
	if prepared == nil || machine == nil || machine.GetUID() == "" {
		return fail(fmt.Errorf("receiver plan requires prepared state"))
	}
	mesh := &corev1.Secret{}
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
		return fail(err)
	}
	adoption := &corev1.Secret{}
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: tunnel.AdoptionSecretName(machine.GetName())}, adoption); err != nil {
		return fail(err)
	}
	if string(mesh.UID) != prepared.Target.MeshUID || mesh.ResourceVersion != prepared.MeshVersion || mesh.DeletionTimestamp != nil || string(adoption.UID) != prepared.Target.SecretUID || adoption.ResourceVersion != prepared.AdoptionVersion || adoption.DeletionTimestamp != nil {
		return fail(fmt.Errorf("receiver preparation snapshot changed"))
	}
	if err := attachment.CheckPeerPublication(ctx, r.reader, machine); err != nil {
		return fail(err)
	}
	node := &corev1.Node{}
	if err := r.reader.Get(ctx, types.NamespacedName{Name: prepared.Target.NodeName}, node); err != nil {
		return fail(err)
	}
	currentMachine := &unstructured.Unstructured{}
	currentMachine.SetGroupVersionKind(machine.GroupVersionKind())
	if err := r.reader.Get(ctx, client.ObjectKeyFromObject(machine), currentMachine); err != nil {
		return fail(err)
	}
	provider, _, _ := unstructured.NestedString(currentMachine.Object, "spec", "providerID")
	nodeRefUID, _, _ := unstructured.NestedString(currentMachine.Object, "status", "nodeRef", "uid")
	if currentMachine.GetUID() != machine.GetUID() || provider == "" || node.Spec.ProviderID != provider || (nodeRefUID != "" && nodeRefUID != string(node.UID)) {
		return fail(fmt.Errorf("receiver machine and node binding changed"))
	}
	target := prepared.Target
	target.Ready = false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			target.Ready = true
		}
	}
	if string(node.UID) != target.NodeUID || node.Status.NodeInfo.BootID != target.BootID || node.DeletionTimestamp != nil {
		return fail(fmt.Errorf("receiver cloud node changed"))
	}
	requestRaw := adoption.Data[tunnel.ReceivePrepareKey]
	base := adoption.Data[tunnel.CloudPeersKey]
	if tunnel.HashPeerList(requestRaw) != prepared.RequestHash || tunnel.HashPeerList(base) != prepared.BaseHash || adoption.Annotations[tunnel.AppliedListAnnotation] != prepared.BaseHash {
		return fail(fmt.Errorf("receiver proposal or forwarding base changed"))
	}
	bindings, err := tunnel.ValidateReceiveReadiness(target, base, requestRaw, adoption.Data[tunnel.ReceiveReadyKey])
	if err != nil {
		return fail(err)
	}
	var request tunnel.ReceivePrepare
	if err := json.Unmarshal(requestRaw, &request); err != nil {
		return fail(err)
	}
	ready, err := r.siteReceivePathsReady(ctx, mesh, adoption, request, bindings, now)
	if err != nil {
		return fail(err)
	}
	if !ready {
		return fail(fmt.Errorf("receiver site preparation is pending"))
	}
	plan := tunnel.ReceiveTransitionPlan{Version: 1, Namespace: r.secretNamespace, MeshName: mesh.Name, MachineName: machine.GetName(), AdoptionName: adoption.Name, CloudNodeName: node.Name, MeshUID: string(mesh.UID), MachineUID: string(machine.GetUID()), AdoptionUID: string(adoption.UID), CloudNodeUID: target.NodeUID, RootPublicKey: target.RootPublicKey, Base: append([]byte(nil), base...), Desired: request.Desired, Native: request.Native, Ports: request.Ports, Bindings: bindings, Sites: map[string]tunnel.ReceiveTransitionSite{}}
	plan.CloudAddresses = receiveMachineAddresses(currentMachine)
	if len(plan.CloudAddresses) == 0 {
		return fail(fmt.Errorf("receiver plan requires the machine's allocated tunnel identity"))
	}
	var previous tunnel.PeerListDoc
	if err := json.Unmarshal(base, &previous); err != nil {
		return fail(err)
	}
	for public := range bindings {
		var receipt tunnel.SiteReceiveReady
		if err := json.Unmarshal(adoption.Data[tunnel.SiteReceiveReadyKey(public)], &receipt); err != nil {
			return fail(err)
		}
		legacy, err := tunnel.PlanReceiveSiteLegacy(mesh.Data, receipt.NodeName, public, plan.RootPublicKey, previous)
		if err != nil {
			return fail(err)
		}
		forward, err := tunnel.PlanReceiveSitePath(mesh.Data, receipt.NodeName, public, plan.RootPublicKey, plan.Desired)
		if err != nil {
			return fail(err)
		}
		plan.Sites[public] = tunnel.ReceiveTransitionSite{NodeName: receipt.NodeName, NodeUID: receipt.NodeUID, RouteSources: tunnel.ReceiveSiteRouteSources(mesh.Data, receipt.NodeName), Client: receipt.Client, Legacy: &legacy, Forward: &forward}
	}
	if len(plan.Sites) == 0 {
		return fail(fmt.Errorf("receiver transition has no site paths"))
	}
	sitePeers, err := tunnel.SitePeers(mesh.Data)
	if err != nil {
		return fail(err)
	}
	plan.SiteDesired = tunnel.PeerListDoc{Peers: sitePeers}
	if _, err := tunnel.PlanReceiveRoutes(plan); err != nil {
		return fail(err)
	}
	// Node and Secret snapshots above remain explicit in the plan and round.
	// Runtime admission must revalidate them; journal creation does not fence nodes.
	return plan, nil
}

func receiveMachineAddresses(machine *unstructured.Unstructured) []string {
	var result []string
	for _, annotation := range []string{join.WireGuardAddrAnnotation, join.WireGuardAddr6Annotation} {
		if address := strings.TrimSpace(machine.GetAnnotations()[annotation]); address != "" {
			result = append(result, address)
		}
	}
	return result
}
