package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
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
	now := time.Now()
	snapshot, err := r.ensureReceiveTransition(ctx, machine, store, p.NativeCIDRs, p.Ports, policy, now)
	if err != nil || snapshot == nil {
		return err
	}
	return recoverExpiredReceiveRound(ctx, store, *snapshot, p.Journal.ExpiryRecovery, now)
}

// recoverPublishedReceiveTransition can run when ordinary CNI reconciliation
// fails. It must not prepare devices, create journals, or publish pending intent.
func (r *meshReconciler) recoverPublishedReceiveTransition(ctx context.Context, machine *unstructured.Unstructured, now time.Time) error {
	p := r.receivePreparation
	if p == nil || p.Journal == nil || p.Journal.ExpiryRecovery == "" {
		return nil
	}
	if _, err := p.Journal.policy(); err != nil {
		return err
	}
	if machine == nil {
		return fmt.Errorf("receiver recovery requires a machine identity")
	}
	store := r.receiveJournal
	store.Name = tunnel.ReceiveTransitionName(string(machine.GetUID()))
	mesh, err := r.receiveTransitionIdentity(ctx, machine, store)
	if err != nil {
		return err
	}
	inventory, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	publication, found := inventory.Publications[string(machine.GetUID())]
	if !found || publication.JournalUID == "" {
		return nil
	}
	store.ExpectedUID = publication.JournalUID
	snapshot, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if err := checkReceiveTransitionIdentity(snapshot, store.ClusterUID, machine, mesh); err != nil {
		return err
	}
	return recoverExpiredReceiveRound(ctx, store, snapshot, p.Journal.ExpiryRecovery, now)
}

// recoverExpiredReceiveRound is opt-in controller policy, separate from journal
// discovery. Recovery appends one fenced event; it never supplies phase receipts.
func recoverExpiredReceiveRound(ctx context.Context, store handover.Store, snapshot handover.Snapshot, mode string, now time.Time) error {
	if mode == "" {
		return nil
	}
	if mode != "rollback" {
		return fmt.Errorf("unsupported receiver expiry recovery policy")
	}
	transition := snapshot.Transition()
	view, err := transition.View()
	if err != nil {
		return err
	}
	if view.Complete || now.Before(view.Round.ExpiresAt) {
		return nil
	}
	var next handover.Transition
	if !view.RollbackRequested && (view.Round.Phase == handover.Prepare || view.Round.Phase == handover.Switch || view.Round.Phase == handover.Drain) {
		next, err = transition.Rollback(now)
	} else {
		// Retirement is irreversible. Already requested rollback must retain its
		// direction, including abort, while collecting fresh observations.
		next, err = transition.Renew(now)
	}
	if err != nil {
		return err
	}
	_, err = store.Commit(ctx, snapshot, next)
	return err
}

// ensureReceiveTransition resumes committed state before considering a new
// proposal. It neither advances phases nor rewrites an unfinished journal.
// Callers must authenticate fresh phase receipts and apply native actions only
// from committed rounds. A completed journal must be archived before reuse.
func (r *meshReconciler) ensureReceiveTransition(ctx context.Context, machine *unstructured.Unstructured, store handover.Store, native []netip.Prefix, ports []int, policy handover.Policy, now time.Time) (*handover.Snapshot, error) {
	mesh, err := r.receiveTransitionIdentity(ctx, machine, store)
	if err != nil {
		return nil, err
	}
	existing, err := store.Load(ctx)
	if err == nil {
		if err := tunnel.ValidateReceiveTransportHistory(ctx, store, existing); err != nil {
			return nil, err
		}
		if err := checkReceiveTransitionIdentity(existing, store.ClusterUID, machine, mesh); err != nil {
			return nil, err
		}
		if err := tunnel.ReserveReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, string(machine.GetUID()), r.owners()); err != nil {
			return nil, err
		}
		existing, err = tunnel.PublishReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, existing, now)
		if err != nil {
			return nil, err
		}
		if existing.Frozen() {
			// Retirement retains the serving plan until participants acknowledge
			// native withdrawal. Source changes cannot start another generation.
			return &existing, nil
		}
		if err := checkCompletedReceiveSources(existing, store.ClusterUID, mesh); err != nil {
			if !errors.Is(err, errReceiveSourcesChanged) || native == nil || ports == nil {
				return nil, err
			}
			return r.prepareAndPublishReceiveSuccessor(ctx, machine, store, existing, native, ports, policy, now)
		}
		return &existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	inventory, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err == nil && slices.Contains(inventory.MachineUIDs, string(machine.GetUID())) {
		publication, found := inventory.Publications[string(machine.GetUID())]
		if !found || publication.JournalUID != "" || len(publication.Plan) == 0 {
			return nil, fmt.Errorf("reserved journal disappeared without an unpublished preparation")
		}
		return r.createPreparedReceiveJournal(ctx, machine, store, mesh, publication.Plan, policy, now)
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
	return r.createPreparedReceiveJournal(ctx, machine, store, mesh, raw, policy, now)
}

func (r *meshReconciler) createPreparedReceiveJournal(ctx context.Context, machine *unstructured.Unstructured, store handover.Store, mesh *corev1.Secret, raw []byte, policy handover.Policy, now time.Time) (*handover.Snapshot, error) {
	var plan tunnel.ReceiveTransitionPlan
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("trailing reserved preparation data")
	}
	if plan.MachineUID != string(machine.GetUID()) || plan.MachineName != machine.GetName() || plan.Namespace != mesh.Namespace || plan.MeshUID != string(mesh.UID) || plan.MeshName != mesh.Name {
		return nil, fmt.Errorf("reserved preparation scope changed")
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
	// Preparation is additive and still needs ordinary agent reconciliation.
	// Reserving earlier would fence those agents on a missing journal before
	// they could publish the readiness needed to construct it. Preserve the
	// reservation-before-creation invariant once the complete plan is ready.
	if err := tunnel.ReservePreparedReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, string(machine.GetUID()), r.owners(), raw); err != nil {
		return nil, err
	}
	store.OwnerReferences = r.owners()
	persisted, err := store.CreateWithPlan(ctx, transition, raw)
	if err != nil {
		return nil, err
	}
	persisted, err = tunnel.PublishReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, persisted, now)
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
	plan := tunnel.ReceiveTransitionPlan{Version: 1, ReceiverGeneration: request.ReceiverGeneration, Namespace: r.secretNamespace, MeshName: mesh.Name, MachineName: machine.GetName(), AdoptionName: adoption.Name, CloudNodeName: node.Name, MeshUID: string(mesh.UID), MachineUID: string(machine.GetUID()), AdoptionUID: string(adoption.UID), CloudNodeUID: target.NodeUID, RootPublicKey: target.RootPublicKey, Base: append([]byte(nil), base...), Desired: request.Desired, Native: request.Native, Ports: request.Ports, Bindings: bindings, Sites: map[string]tunnel.ReceiveTransitionSite{}}
	plan.CloudAddresses = receiveMachineAddresses(currentMachine)
	if len(plan.CloudAddresses) == 0 {
		return fail(fmt.Errorf("receiver plan requires the machine's allocated tunnel identity"))
	}
	var previous tunnel.PeerListDoc
	if err := json.Unmarshal(base, &previous); err != nil {
		return fail(err)
	}
	plan.SiteBase, err = r.receivePreparationSiteBase(ctx, adoption, request)
	if err != nil {
		return fail(err)
	}
	predecessor, err := r.receivePreparationPredecessor(ctx, adoption, request)
	if err != nil {
		return fail(err)
	}
	if predecessor != nil {
		old, err := request.ValidatePredecessor(*predecessor)
		if err != nil {
			return fail(err)
		}
		view, err := predecessor.Transition().View()
		if err != nil {
			return fail(err)
		}
		serving, root, err := old.SelectedReceiveTransport(view)
		if err != nil {
			return fail(err)
		}
		if !root {
			serving.Version, serving.PreviousReceiver = 1, nil
			plan.Version = 2
			plan.PreviousReceiver = &tunnel.ReceivePreviousReceiver{Generation: view.ServingGeneration, Plan: &serving}
		}
	}
	// The legacy projection is still validated against the acknowledged base,
	// but its worker destinations belong to the predecessor's site document.
	// Current publication may already contain a newly allocated pod range.
	legacyPeers, err := tunnel.SitePeers(mesh.Data)
	if err != nil {
		return fail(err)
	}
	if plan.SiteBase != nil {
		legacyPeers = plan.SiteBase.Peers
	}
	var legacyCloud []tunnel.PeerSpec
	for _, peer := range legacyPeers {
		if peer.PublicKey == plan.RootPublicKey {
			legacyCloud = append(legacyCloud, peer)
		}
	}
	if len(legacyCloud) != 1 {
		return fail(fmt.Errorf("predecessor cloud destinations missing or ambiguous"))
	}
	for public := range bindings {
		var receipt tunnel.SiteReceiveReady
		if err := json.Unmarshal(adoption.Data[tunnel.SiteReceiveReadyKey(public)], &receipt); err != nil {
			return fail(err)
		}
		legacy, err := tunnel.PlanReceiveSitePathForPeer(mesh.Data, receipt.NodeName, public, previous, legacyCloud[0])
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

func (r *meshReconciler) receiveTransitionIdentity(ctx context.Context, machine *unstructured.Unstructured, store handover.Store) (*corev1.Secret, error) {
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
	return mesh, nil
}

func checkReceiveTransitionIdentity(snapshot handover.Snapshot, clusterUID string, machine *unstructured.Unstructured, mesh *corev1.Secret) error {
	plan, err := tunnel.DecodeReceiveTransition(snapshot, clusterUID)
	if err != nil {
		return err
	}
	if plan.MachineUID != string(machine.GetUID()) || plan.MachineName != machine.GetName() || plan.Namespace != mesh.Namespace || plan.MeshUID != string(mesh.UID) || plan.MeshName != mesh.Name {
		return fmt.Errorf("receiver journal belongs to another machine")
	}
	return nil
}

// checkCompletedReceiveSources prevents a terminal journal from reporting
// convergence for allocations it never admitted. It does not widen native
// permissions or authorize replacing the serving generation.
func checkCompletedReceiveSources(snapshot handover.Snapshot, clusterUID string, mesh *corev1.Secret) error {
	view, err := snapshot.Transition().View()
	if err != nil || !view.Complete {
		return err
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, clusterUID)
	if err != nil {
		return err
	}
	serving, err := plan.CompletedSitePeers(view)
	if err != nil {
		return err
	}
	published, err := tunnel.SitePeers(mesh.Data)
	if err != nil {
		return err
	}
	sources := func(peers []tunnel.PeerSpec) ([]string, error) {
		var result []string
		found := false
		for _, peer := range peers {
			if peer.PublicKey != plan.RootPublicKey {
				continue
			}
			if found {
				return nil, fmt.Errorf("duplicate completed receiver source owner")
			}
			found = true
			for _, raw := range peer.WGAllowedIPs {
				prefix, err := netip.ParsePrefix(raw)
				if err != nil {
					return nil, fmt.Errorf("invalid completed receiver source prefix")
				}
				result = append(result, prefix.Masked().String())
			}
		}
		if !found {
			return nil, fmt.Errorf("completed receiver source owner is missing")
		}
		slices.Sort(result)
		return slices.Compact(result), nil
	}
	want, err := sources(published)
	if err != nil {
		return err
	}
	active, err := sources(serving.Peers)
	if err != nil {
		return err
	}
	if !slices.Equal(want, active) {
		return errReceiveSourcesChanged
	}
	return nil
}

var errReceiveSourcesChanged = errors.New("completed receive transition does not cover current worker sources; successor transition required")

// Retain the published journal UID and inventory reservation. Archive access
// precedes replacement, so agents can authenticate the new generation's ancestry.
// A failed preparation, archive, Role update or CAS leaves serving authority in
// the old journal; retries use the same proposal and immutable archive.
func (r *meshReconciler) prepareAndPublishReceiveSuccessor(ctx context.Context, machine *unstructured.Unstructured, store handover.Store, previous handover.Snapshot, native []netip.Prefix, ports []int, policy handover.Policy, now time.Time) (*handover.Snapshot, error) {
	prepared, err := r.prepareRemoteReceiveGeneration(ctx, machine, native, ports, &previous)
	if err != nil || prepared == nil {
		return nil, err
	}
	if r.dialerServiceAccount == "" || r.receivePreparation == nil || r.receivePreparation.Journal == nil || r.receiveJournal.ClusterUID != store.ClusterUID {
		return nil, fmt.Errorf("successor publication requires configured agent archive access")
	}
	if _, err := store.ArchiveCompleted(ctx, previous); err != nil {
		return nil, err
	}
	if err := r.ensureDialerRole(ctx); err != nil {
		return nil, err
	}
	// Role reconciliation may take time. Revalidate the exact preparation
	// versions and predecessor after granting access, before committing a plan.
	plan, err := r.buildReceiveTransitionPlan(ctx, machine, prepared, time.Now())
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	spec, err := tunnel.ReceiveTransitionSpec(plan, store.ClusterUID, fmt.Sprintf("%x", sha256.Sum256(raw)))
	if err != nil {
		return nil, err
	}
	persisted, err := store.ReplaceCompleted(ctx, previous, spec, raw, now, policy)
	if err != nil {
		return nil, err
	}
	if err := tunnel.ValidateReceiveTransportHistory(ctx, store, persisted); err != nil {
		return nil, err
	}
	return &persisted, nil
}
