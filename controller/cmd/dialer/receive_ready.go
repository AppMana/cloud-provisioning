package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

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
	base := secret.Data[tunnel.CloudPeersKey]
	if len(base) == 0 || request.BaseHash != tunnel.HashPeerList(base) {
		return fmt.Errorf("receive preparation base changed")
	}
	var previous tunnel.PeerListDoc
	if err := json.Unmarshal(base, &previous); err != nil {
		return fmt.Errorf("invalid receive preparation base")
	}
	plan, err := tunnel.PlanReceiveHandover(request.MeshUID, previous, request.Desired, request.Native)
	if err != nil {
		return err
	}
	node, err := k.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(node.UID) != request.NodeUID || node.DeletionTimestamp != nil {
		return fmt.Errorf("receive preparation node identity changed")
	}
	bindings, err := prepare(request, previous)
	if err != nil {
		return err
	}
	// Check completeness before publishing; no partial preparation is readiness.
	if len(bindings) != len(plan.Receivers) {
		return fmt.Errorf("incomplete receiver preparation")
	}
	byPeer := map[string]receiveBinding{}
	publicKeys := map[string]bool{}
	for _, binding := range bindings {
		key, err := wgtypes.ParseKey(binding.PublicKey)
		if err != nil || key == (wgtypes.Key{}) || key.String() != binding.PublicKey || binding.PublicKey == request.RootPublicKey || publicKeys[binding.PublicKey] || binding.ListenPort <= 0 || binding.ListenPort > 65535 {
			return fmt.Errorf("invalid prepared receiver binding")
		}
		if _, found := byPeer[binding.PeerPublicKey]; found {
			return fmt.Errorf("duplicate prepared receiver")
		}
		byPeer[binding.PeerPublicKey] = binding
		publicKeys[binding.PublicKey] = true
	}
	for _, path := range plan.Receivers {
		if binding, found := byPeer[path.PublicKey]; !found || binding.Interface != path.Interface {
			return fmt.Errorf("prepared receiver identity mismatch")
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
	if string(node.UID) != request.NodeUID || node.DeletionTimestamp != nil {
		return fmt.Errorf("receive preparation node changed before publication")
	}
	receipt := tunnel.ReceiveReady{Version: 1, MeshUID: request.MeshUID, SecretUID: string(secret.UID), NodeName: nodeName, NodeUID: request.NodeUID, RootPublicKey: request.RootPublicKey, BaseHash: request.BaseHash, RequestHash: tunnel.HashPeerList(raw), Bindings: bindings}
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
