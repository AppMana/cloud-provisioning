package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

func localReceiveBootID() (string, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("reading receiver boot identity: %w", err)
	}
	boot := strings.TrimSpace(string(raw))
	if boot == "" {
		return "", fmt.Errorf("receiver boot identity is missing")
	}
	return boot, nil
}

type prepareReceivers func(tunnel.ReceivePrepare, tunnel.PeerListDoc) ([]receiveBinding, error)

// reconcileReceivePreparation never changes the active forwarding document or
// applied annotation. The caller has already reconciled the base document. A
// stale request can leave additive receivers, but cannot publish a ready receipt.
func reconcileReceivePreparation(ctx context.Context, k kubernetes.Interface, secret *corev1.Secret, nodeName string, root wgtypes.Key, prepare prepareReceivers) error {
	raw, requested := secret.Data[tunnel.ReceivePrepareKey]
	if !requested {
		return nil
	}
	if secret.UID == "" || secret.ResourceVersion == "" || secret.DeletionTimestamp != nil {
		return fmt.Errorf("receive preparation requires a live versioned Secret")
	}
	var request tunnel.ReceivePrepare
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("invalid receive preparation request")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing receive preparation input")
	}
	if request.Version != 1 || request.MeshUID == "" || request.NodeName == "" || request.NodeName != nodeName || request.NodeUID == "" || root == (wgtypes.Key{}) || request.RootPublicKey != root.PublicKey().String() || request.Native == nil {
		return fmt.Errorf("receive preparation identity or protocol mismatch")
	}
	bootID, err := localReceiveBootID()
	if err != nil {
		return err
	}
	if request.BootID != bootID {
		return fmt.Errorf("receive preparation targets another kernel boot")
	}
	base := secret.Data[tunnel.CloudPeersKey]
	if len(base) == 0 || request.BaseHash != tunnel.HashPeerList(base) {
		return fmt.Errorf("receive preparation base changed")
	}
	var previous tunnel.PeerListDoc
	if err := json.Unmarshal(base, &previous); err != nil {
		return fmt.Errorf("invalid receive preparation base")
	}
	receiveScope, err := request.ReceiveScope()
	if err != nil {
		return err
	}
	plan, err := tunnel.PlanReceiveHandover(receiveScope, previous, request.Desired, request.Native)
	if err != nil {
		return err
	}
	if err := tunnel.ValidateReceivePorts(plan.Receivers, request.Ports); err != nil {
		return err
	}
	node, err := k.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(node.UID) != request.NodeUID || node.Status.NodeInfo.BootID != bootID || node.DeletionTimestamp != nil {
		return fmt.Errorf("receive preparation node identity changed")
	}
	if err := checkReceivePreparationPredecessor(ctx, k, secret, request); err != nil {
		return err
	}
	bindings, err := prepare(request, previous)
	if err != nil {
		return err
	}
	if err := tunnel.ValidateReceiveBindings(plan.Receivers, request.RootPublicKey, bindings); err != nil {
		return err
	}
	for _, binding := range bindings {
		if binding.ListenPort != request.Ports[binding.PeerPublicKey] {
			return fmt.Errorf("prepared listener does not match its assigned port")
		}
	}
	current, err := k.CoreV1().Secrets(secret.Namespace).Get(ctx, secret.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if current.UID != secret.UID || current.ResourceVersion != secret.ResourceVersion || current.DeletionTimestamp != nil || !bytes.Equal(current.Data[tunnel.ReceivePrepareKey], raw) || !bytes.Equal(current.Data[tunnel.CloudPeersKey], base) {
		return fmt.Errorf("receive preparation Secret changed before publication")
	}
	node, err = k.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(node.UID) != request.NodeUID || node.Status.NodeInfo.BootID != bootID || node.DeletionTimestamp != nil {
		return fmt.Errorf("receive preparation node changed before publication")
	}
	if err := checkReceivePreparationPredecessor(ctx, k, current, request); err != nil {
		return err
	}
	receipt := tunnel.ReceiveReady{Version: 1, MeshUID: request.MeshUID, SecretUID: string(secret.UID), NodeName: nodeName, NodeUID: request.NodeUID, BootID: bootID, RootPublicKey: request.RootPublicKey, BaseHash: request.BaseHash, RequestHash: tunnel.HashPeerList(raw), Bindings: bindings}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if bytes.Equal(current.Data[tunnel.ReceiveReadyKey], encoded) {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": current.UID, "resourceVersion": current.ResourceVersion}, "data": map[string][]byte{tunnel.ReceiveReadyKey: encoded}})
	if err != nil {
		return err
	}
	_, err = k.CoreV1().Secrets(secret.Namespace).Patch(ctx, secret.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// A changed predecessor may leave an unused additive device, but cannot earn
// readiness. Journal reservation still fences the ordinary preparation path.
func checkReceivePreparationPredecessor(ctx context.Context, k kubernetes.Interface, secret *corev1.Secret, request tunnel.ReceivePrepare) error {
	_, err := receivePreparationPreviousSitePeers(ctx, k, secret, request)
	return err
}

// Nil preserves the original preparation protocol. A successor uses the exact
// serving predecessor's permissions, independently of the current publication.
func receivePreparationPreviousSitePeers(ctx context.Context, k kubernetes.Interface, secret *corev1.Secret, request tunnel.ReceivePrepare) (*tunnel.PeerListDoc, error) {
	if request.Predecessor == nil {
		return nil, nil
	}
	ref := request.Predecessor
	store := handover.Store{ConfigMaps: k.CoreV1().ConfigMaps(secret.Namespace), Name: tunnel.ReceiveTransitionName(ref.MachineUID), ClusterUID: ref.ClusterUID, ExpectedUID: ref.JournalUID}
	snapshot, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := request.ValidatePredecessor(snapshot)
	if err != nil {
		return nil, err
	}
	if plan.Namespace != secret.Namespace || plan.AdoptionName != secret.Name || plan.AdoptionUID != string(secret.UID) {
		return nil, fmt.Errorf("successor preparation adoption identity changed")
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
