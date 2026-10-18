package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type preparedRemoteReceivers struct {
	Target          tunnel.ReceiveTarget
	AdoptionVersion string
	MeshVersion     string
	RequestHash     string
	BaseHash        string
	Bindings        map[string]tunnel.ReceiveBinding
}

// prepareRemoteReceivePaths stages the controller's current future document
// without changing the remote's active peers.json. The caller supplies verified
// native network boundaries and a permitted UDP listener pool. Unknown policy
// cannot authorize preparation. Port reservations survive proposal changes.
// A nil result means preparation is pending. Returned bindings are preparation
// evidence only, never permission to move traffic or release old endpoints.
// Activation orchestration must gate entry into this phase on agent support.
func (r *meshReconciler) prepareRemoteReceivePaths(ctx context.Context, expected *unstructured.Unstructured, native []netip.Prefix, portPool []int) (*preparedRemoteReceivers, error) {
	return r.prepareRemoteReceiveGeneration(ctx, expected, native, portPool, nil)
}

// A successor uses the same proposal/readiness exchange, but binds additive
// preparation to a terminal journal and reserves distinct generation listeners.
// The caller still owns journal replacement and publication ordering.
func (r *meshReconciler) prepareRemoteReceiveGeneration(ctx context.Context, expected *unstructured.Unstructured, native []netip.Prefix, portPool []int, predecessor *handover.Snapshot) (*preparedRemoteReceivers, error) {
	if expected == nil || expected.GetUID() == "" || expected.GetNamespace() != r.secretNamespace || native == nil || portPool == nil {
		return nil, fmt.Errorf("receiver preparation requires a scoped Machine and verified native boundaries")
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(expected.GroupVersionKind())
	if err := r.reader.Get(ctx, client.ObjectKeyFromObject(expected), machine); err != nil {
		return nil, err
	}
	if machine.GetUID() != expected.GetUID() {
		return nil, fmt.Errorf("receiver Machine identity changed")
	}
	mesh := &corev1.Secret{}
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
		return nil, err
	}
	if mesh.UID == "" || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil {
		return nil, fmt.Errorf("receiver mesh identity is unavailable")
	}
	if err := attachment.CheckPeerPublication(ctx, r.reader, machine); err != nil {
		return nil, err
	}
	nodeName := r.nodeNameForMachine(ctx, machine)
	if nodeName == "" {
		return nil, nil
	}
	node := &corev1.Node{}
	if err := r.reader.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return nil, err
	}
	provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
	nodeRefUID, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "uid")
	if provider == "" || node.Spec.ProviderID != provider || (nodeRefUID != "" && nodeRefUID != string(node.UID)) {
		return nil, fmt.Errorf("receiver Machine and Node binding changed")
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready || node.UID == "" || node.Status.NodeInfo.BootID == "" || node.DeletionTimestamp != nil {
		return nil, nil
	}
	root := string(mesh.Data[tunnel.PeerPublicKeyPrefix+machine.GetName()])
	key, err := wgtypes.ParseKey(root)
	if err != nil || key == (wgtypes.Key{}) || key.String() != root {
		return nil, fmt.Errorf("receiver root identity is unavailable")
	}
	adoption := &corev1.Secret{}
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: tunnel.AdoptionSecretName(machine.GetName())}, adoption); err != nil {
		return nil, err
	}
	if adoption.UID == "" || adoption.ResourceVersion == "" || adoption.DeletionTimestamp != nil {
		return nil, fmt.Errorf("receiver adoption identity is unavailable")
	}
	baseRaw := adoption.Data[tunnel.CloudPeersKey]
	if len(baseRaw) == 0 {
		return nil, fmt.Errorf("receiver forwarding base is missing")
	}
	// Preparation must start from forwarding the dialer actually acknowledged.
	if adoption.Annotations[tunnel.AppliedListAnnotation] != tunnel.HashPeerList(baseRaw) {
		return nil, nil
	}
	self := strings.SplitN(strings.TrimSpace(machine.GetAnnotations()[join.WireGuardAddrAnnotation]), "/", 2)[0]
	if self == "" {
		return nil, fmt.Errorf("receiver tunnel identity is missing")
	}
	futureRaw, err := tunnel.RemotePeerDocument(mesh.Data, self, r.apiVIP, r.apiServerPort)
	if err != nil {
		return nil, err
	}
	if len(futureRaw) == 0 {
		return nil, nil
	}
	var previous, future tunnel.PeerListDoc
	if err := json.Unmarshal(baseRaw, &previous); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(futureRaw, &future); err != nil {
		return nil, err
	}
	request := tunnel.ReceivePrepare{Version: 1, MeshUID: string(mesh.UID), NodeName: node.Name, NodeUID: string(node.UID), BootID: node.Status.NodeInfo.BootID, RootPublicKey: root, BaseHash: tunnel.HashPeerList(baseRaw), Desired: future, Native: native}
	if predecessor != nil {
		ref, err := tunnel.NewReceivePreparePredecessor(*predecessor, r.receiveJournal.ClusterUID)
		if err != nil {
			return nil, err
		}
		if ref.MachineUID != string(machine.GetUID()) {
			return nil, fmt.Errorf("preparation predecessor belongs to another machine")
		}
		request.Predecessor = &ref
		// Boot/readiness changes must not consume another set of listeners.
		// Bind the desired transport and this worker's site permissions, not
		// unrelated workers or transient Secret resource versions.
		peers, err := tunnel.SitePeers(mesh.Data)
		if err != nil {
			return nil, err
		}
		var worker []tunnel.PeerSpec
		for _, peer := range peers {
			if peer.PublicKey == root {
				worker = append(worker, peer)
			}
		}
		if len(worker) != 1 {
			return nil, fmt.Errorf("successor preparation lacks unique worker destinations")
		}
		identity, err := json.Marshal(struct {
			Predecessor tunnel.ReceivePreparePredecessor
			Desired     tunnel.PeerListDoc
			Native      []netip.Prefix
			Worker      tunnel.PeerSpec
		}{ref, future, native, worker[0]})
		if err != nil {
			return nil, err
		}
		request.ReceiverGeneration = fmt.Sprintf("%x", sha256.Sum256(identity))
	}
	receiveScope, err := request.ReceiveScope()
	if err != nil {
		return nil, err
	}
	plan, err := tunnel.PlanReceiveHandover(receiveScope, previous, future, native)
	if err != nil {
		return nil, err
	}
	reservations := tunnel.ReceivePortReservations{Version: 1, MeshUID: string(mesh.UID), NodeUID: string(node.UID), RootPublicKey: root, Ports: map[string]int{}}
	if raw, found := adoption.Data[tunnel.ReceivePortsKey]; found {
		if err := json.Unmarshal(raw, &reservations); err != nil {
			return nil, fmt.Errorf("invalid receive port reservations")
		}
		if reservations.Validate() != nil || reservations.MeshUID != string(mesh.UID) || reservations.NodeUID != string(node.UID) || reservations.RootPublicKey != root {
			return nil, fmt.Errorf("receive port reservation identity changed")
		}
	} else if len(adoption.Data[tunnel.ReceivePrepareKey]) > 0 {
		return nil, fmt.Errorf("existing receive proposal has no port reservations")
	}
	ports, retained, err := reservations.Assign(request.ReceiverGeneration, plan.Receivers, portPool)
	if err != nil {
		return nil, err
	}
	reservations = retained
	reservationRaw, err := json.Marshal(reservations)
	if err != nil {
		return nil, err
	}
	request.Ports = ports
	if predecessor != nil {
		if _, err := request.ValidatePredecessor(*predecessor); err != nil {
			return nil, err
		}
		if _, err := r.receivePreparationSiteBase(ctx, adoption, request); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	// Refresh independent identities before either accepting readiness or writing
	// the proposal. The adoption patch itself uses optimistic concurrency.
	currentMesh := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKeyFromObject(mesh), currentMesh); err != nil {
		return nil, err
	}
	if currentMesh.UID != mesh.UID || currentMesh.ResourceVersion != mesh.ResourceVersion || currentMesh.DeletionTimestamp != nil {
		return nil, fmt.Errorf("receiver mesh changed during preparation")
	}
	if err := attachment.CheckPeerPublication(ctx, r.reader, machine); err != nil {
		return nil, err
	}
	currentNode := &corev1.Node{}
	if err := r.reader.Get(ctx, client.ObjectKeyFromObject(node), currentNode); err != nil {
		return nil, err
	}
	currentReady := false
	for _, condition := range currentNode.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			currentReady = true
		}
	}
	if currentNode.UID != node.UID || currentNode.Status.NodeInfo.BootID != node.Status.NodeInfo.BootID || currentNode.Spec.ProviderID != provider || !currentReady || currentNode.DeletionTimestamp != nil {
		return nil, fmt.Errorf("receiver Node changed during preparation")
	}
	if predecessor != nil {
		if _, err := r.receivePreparationSiteBase(ctx, adoption, request); err != nil {
			return nil, err
		}
	}
	if !bytes.Equal(adoption.Data[tunnel.ReceivePrepareKey], raw) || !bytes.Equal(adoption.Data[tunnel.ReceivePortsKey], reservationRaw) {
		patch := client.MergeFromWithOptions(adoption.DeepCopy(), client.MergeFromWithOptimisticLock{})
		adoption.Data[tunnel.ReceivePrepareKey] = raw
		adoption.Data[tunnel.ReceivePortsKey] = reservationRaw
		delete(adoption.Data, tunnel.ReceiveReadyKey)
		if err := r.Patch(ctx, adoption, patch); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if len(adoption.Data[tunnel.ReceiveReadyKey]) == 0 {
		return nil, nil
	}
	target := tunnel.ReceiveTarget{MeshUID: string(mesh.UID), SecretUID: string(adoption.UID), NodeName: node.Name, NodeUID: string(node.UID), BootID: node.Status.NodeInfo.BootID, RootPublicKey: root, Ready: ready}
	bindings, err := tunnel.ValidateReceiveReadiness(target, baseRaw, raw, adoption.Data[tunnel.ReceiveReadyKey])
	if err != nil {
		return nil, err
	}
	if ready, err := r.siteReceivePathsReady(ctx, mesh, adoption, request, bindings, time.Now()); err != nil || !ready {
		return nil, err
	}
	return &preparedRemoteReceivers{Target: target, MeshVersion: mesh.ResourceVersion, AdoptionVersion: adoption.ResourceVersion, RequestHash: tunnel.HashPeerList(raw), BaseHash: request.BaseHash, Bindings: bindings}, nil
}

// Require every site's authenticated transport, including retained senders.
// A site leaving desired placement still participates in switch/drain/retire.
func (r *meshReconciler) siteReceivePathsReady(ctx context.Context, mesh, adoption *corev1.Secret, request tunnel.ReceivePrepare, bindings map[string]tunnel.ReceiveBinding, now time.Time) (bool, error) {
	sitePeers, err := tunnel.SitePeers(mesh.Data)
	if err != nil {
		return false, err
	}
	siteDoc := tunnel.PeerListDoc{Peers: sitePeers}
	previous := siteDoc
	predecessor, err := r.receivePreparationSiteBase(ctx, adoption, request)
	if err != nil {
		return false, err
	}
	if predecessor != nil {
		previous = *predecessor
	}

	siteHash, err := tunnel.SitePeerHash(mesh.Data)
	if err != nil {
		return false, err
	}
	names := map[string]string{}
	for key, value := range mesh.Data {
		if !strings.HasPrefix(key, tunnel.NodePublicKeyPrefix) {
			continue
		}
		public := strings.TrimSpace(string(value))
		if _, exists := names[public]; exists && public != "" {
			return false, fmt.Errorf("duplicate site receiver identity")
		}
		names[public] = strings.TrimPrefix(key, tunnel.NodePublicKeyPrefix)
	}
	keys := make([]string, 0, len(bindings))
	for public := range bindings {
		keys = append(keys, public)
	}
	sort.Strings(keys)
	for _, public := range keys {
		name := names[public]
		if name == "" {
			return false, fmt.Errorf("participating site receiver has no current node")
		}
		binding := bindings[public]
		raw := adoption.Data[tunnel.SiteReceiveReadyKey(public)]
		if len(raw) == 0 {
			return false, nil
		}
		node := &corev1.Node{}
		if err := r.reader.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
			return false, err
		}
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready || node.UID == "" || node.Status.NodeInfo.BootID == "" || node.DeletionTimestamp != nil {
			return false, nil
		}
		receiveScope, err := request.ReceiveScope()
		if err != nil {
			return false, err
		}
		plan, err := tunnel.PlanSiteReceiveClient(receiveScope, public, request.RootPublicKey, binding, previous, siteDoc)
		if err != nil {
			return false, err
		}
		expected := tunnel.SiteReceiveReady{Version: 1, MeshUID: string(mesh.UID), SecretUID: string(adoption.UID), RequestHash: tunnel.HashPeerList(adoption.Data[tunnel.ReceivePrepareKey]), SiteHash: siteHash, NodeName: node.Name, NodeUID: string(node.UID), BootID: node.Status.NodeInfo.BootID, Client: plan}
		if err := tunnel.ValidateSiteReceiveReadiness(expected, raw, now); err != nil {
			return false, err
		}
	}
	// Site reads can take multiple API round trips. Return only a still-current
	// proposal snapshot; activation must also check these versions when writing.
	currentMesh := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKeyFromObject(mesh), currentMesh); err != nil {
		return false, err
	}
	currentAdoption := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKeyFromObject(adoption), currentAdoption); err != nil {
		return false, err
	}
	if currentMesh.UID != mesh.UID || currentMesh.ResourceVersion != mesh.ResourceVersion || currentMesh.DeletionTimestamp != nil || currentAdoption.UID != adoption.UID || currentAdoption.ResourceVersion != adoption.ResourceVersion || currentAdoption.DeletionTimestamp != nil {
		return false, fmt.Errorf("receiver proposal changed during site readiness validation")
	}
	if _, err := r.receivePreparationSiteBase(ctx, adoption, request); err != nil {
		return false, err
	}
	return true, nil
}

// Load terminal source permissions using the configured cluster authority.
// This does not authorize replacing the journal or its serving transport graph.
func (r *meshReconciler) receivePreparationSiteBase(ctx context.Context, adoption *corev1.Secret, request tunnel.ReceivePrepare) (*tunnel.PeerListDoc, error) {
	snapshot, err := r.receivePreparationPredecessor(ctx, adoption, request)
	if err != nil || snapshot == nil {
		return nil, err
	}
	plan, err := request.ValidatePredecessor(*snapshot)
	if err != nil {
		return nil, err
	}
	view, err := snapshot.Transition().View()
	if err != nil {
		return nil, err
	}
	previous, err := plan.CompletedSitePeers(view)
	if err != nil {
		return nil, err
	}
	return &previous, nil
}

func (r *meshReconciler) receivePreparationPredecessor(ctx context.Context, adoption *corev1.Secret, request tunnel.ReceivePrepare) (*handover.Snapshot, error) {
	if request.Predecessor == nil {
		return nil, nil
	}
	ref := request.Predecessor
	store := r.receiveJournal
	if store.ConfigMaps == nil || store.ClusterUID == "" || store.ClusterUID != ref.ClusterUID {
		return nil, fmt.Errorf("successor preparation cluster authority unavailable")
	}
	store.Name, store.ExpectedUID = tunnel.ReceiveTransitionName(ref.MachineUID), ref.JournalUID
	snapshot, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := request.ValidatePredecessor(snapshot)
	if err != nil {
		return nil, err
	}
	if plan.Namespace != adoption.Namespace || plan.AdoptionName != adoption.Name || plan.AdoptionUID != string(adoption.UID) {
		return nil, fmt.Errorf("successor preparation adoption identity changed")
	}
	if _, err := store.ArchiveNames(ctx, snapshot); err != nil {
		return nil, err
	}
	if err := tunnel.ValidateReceiveTransportHistory(ctx, store, snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

// Include a pending successor's exact terminal archive in ordinary Role
// reconciliation too, so a retry cannot revoke access between grant and CAS.
// Consumed proposals no longer request an archive; published ancestry owns it.
func (r *meshReconciler) receivePreparationArchive(ctx context.Context, store handover.Store, snapshot handover.Snapshot) (string, error) {
	view, err := snapshot.Transition().View()
	if err != nil || !view.Complete {
		return "", err
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	if err != nil {
		return "", err
	}
	adoption := &corev1.Secret{}
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: plan.Namespace, Name: plan.AdoptionName}, adoption); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil // A retained journal need not still have a proposal Secret.
		}
		return "", err
	}
	raw := adoption.Data[tunnel.ReceivePrepareKey]
	if len(raw) == 0 {
		return "", nil
	}
	var request tunnel.ReceivePrepare
	if err := json.Unmarshal(raw, &request); err != nil {
		return "", err
	}
	if request.Predecessor == nil || request.ReceiverGeneration == plan.ReceiverGeneration {
		return "", nil
	}
	if string(adoption.UID) != plan.AdoptionUID || adoption.DeletionTimestamp != nil {
		return "", fmt.Errorf("pending successor adoption identity changed")
	}
	if _, err := request.ValidatePredecessor(snapshot); err != nil {
		return "", err
	}
	if err := tunnel.ValidateReceiveTransportHistory(ctx, store, snapshot); err != nil {
		return "", err
	}
	archive, err := store.ArchiveCompleted(ctx, snapshot)
	if err != nil {
		return "", err
	}
	return archive.Metadata().Name, nil
}
