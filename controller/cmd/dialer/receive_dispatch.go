package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type receiveDispatchNative func(context.Context, tunnel.ReceiveTransitionPlan, handover.View, string, tunnel.ReceiveJournalRoutes, wgtypes.Key) error

// A local reserved journal owns forwarding even after completion. Never fall
// through to legacy reconciliation on an API, credential, admission or native
// failure. Retiring inventory and restoring ordinary publishers is separate.
func dispatchReceivePhases(ctx context.Context, api kubernetes.Interface, cfg config, native receiveDispatchNative) (bool, error) {
	namespace := cfg.secretNamespace
	if namespace == "" {
		namespace = cfg.peersSecretNamespace
	}
	if api == nil || native == nil || namespace == "" || cfg.receiptURL == "" || cfg.receiptCAFile == "" || cfg.receiptTokenFile == "" || cfg.receiptMeshName == "" || cfg.receiptPodName == "" || cfg.receiptPodUID == "" || cfg.nodeName == "" {
		return true, fmt.Errorf("receiver dispatch requires API, pod identity and complete receipt configuration")
	}
	if cfg.secretName != "" && cfg.secretName != cfg.receiptMeshName {
		return true, fmt.Errorf("receiver discovery differs from configured mesh")
	}
	mesh, err := api.CoreV1().Secrets(namespace).Get(ctx, cfg.receiptMeshName, metav1.GetOptions{})
	if err != nil {
		return true, err
	}
	cluster, err := api.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return true, err
	}
	if mesh.UID == "" || mesh.DeletionTimestamp != nil || cluster.UID == "" || cluster.DeletionTimestamp != nil {
		return true, fmt.Errorf("receiver dispatch requires live mesh and cluster identities")
	}
	store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps(namespace), ClusterUID: string(cluster.UID)}
	inventory, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, namespace, mesh.Name, string(mesh.UID))
	if apierrors.IsNotFound(err) {
		return false, nil
	} // No reserved phase; the legacy host fence still applies.
	if err != nil {
		return true, err
	}
	var targets []string
	for _, uid := range inventory.MachineUIDs {
		store.Name = tunnel.ReceiveTransitionName(uid)
		snapshot, err := store.Load(ctx)
		if err != nil {
			return true, err
		}
		plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
		if err != nil {
			return true, err
		}
		if plan.MachineUID != uid || plan.MeshUID != string(mesh.UID) || plan.Namespace != namespace || plan.MeshName != mesh.Name {
			return true, fmt.Errorf("receiver discovery scope changed")
		}
		local := plan.CloudNodeName == cfg.nodeName
		for _, site := range plan.Sites {
			local = local || site.NodeName == cfg.nodeName
		}
		if local {
			targets = append(targets, uid)
		}
	}
	if len(targets) == 0 {
		return false, nil
	}
	key, allocated, err := receiveDispatchIdentity(cfg)
	if err != nil {
		return true, err
	}
	ca, err := os.ReadFile(cfg.receiptCAFile)
	if err != nil {
		return true, fmt.Errorf("reading receipt trust: %w", err)
	}
	transport, err := handover.NewReceiptClient(cfg.receiptURL, ca)
	if err != nil {
		return true, err
	}
	defer transport.Close()
	var failures []error
	for _, uid := range targets {
		store.Name = tunnel.ReceiveTransitionName(uid)
		scope := receiveJournalScope{Namespace: namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), MachineUID: uid}
		pod, err := api.CoreV1().Pods(namespace).Get(ctx, cfg.receiptPodName, metav1.GetOptions{})
		if err != nil {
			return true, err
		}
		if string(pod.UID) != cfg.receiptPodUID || pod.Spec.NodeName != cfg.nodeName || pod.DeletionTimestamp != nil {
			return true, fmt.Errorf("receipt agent pod identity changed")
		}
		containerID := ""
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "dialer" && status.Ready && status.State.Running != nil {
				containerID = status.ContainerID
			}
		}
		if containerID == "" {
			return true, fmt.Errorf("receipt agent container is not ready")
		}
		token, err := os.ReadFile(cfg.receiptTokenFile)
		if err != nil {
			return true, fmt.Errorf("reading projected receipt credential failed")
		}
		snapshot, err := store.Load(ctx)
		if err != nil {
			return true, err
		}
		view, err := snapshot.Transition().View()
		if err != nil {
			return true, err
		}
		verify := func(ctx context.Context, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes) error {
			_, expected, err := tunnel.ReceiveLocalNetwork(plan, nodeUID)
			if err != nil {
				return err
			}
			if cfg.peersFile != "" && !slices.Equal(allocated, expected) {
				return fmt.Errorf("bootstrap tunnel allocation differs from committed plan")
			}
			return native(ctx, plan, view, nodeUID, intent, key)
		}
		if view.Complete {
			if err := verifyCompletedReceivePhase(ctx, api, store, scope, cfg, key, containerID, verify); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		receipts, err := executeReceivePhase(ctx, api, store, scope, cfg, key, containerID, verify)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if err := transport.Submit(ctx, strings.TrimSpace(string(token)), handover.ReceiptRequest{MachineUID: uid, RoundHash: receipts[0].Receipt.RoundHash, Receipts: receipts}); err != nil {
			failures = append(failures, err)
		}
	}
	return true, errors.Join(failures...)
}

func receiveDispatchIdentity(cfg config) (wgtypes.Key, []string, error) {
	if cfg.peersFile != "" {
		doc, err := readPeersFileDoc(cfg.peersFile)
		if err != nil {
			return wgtypes.Key{}, nil, err
		}
		key, err := wgtypes.ParseKey(doc.PrivateKey)
		if err != nil {
			return wgtypes.Key{}, nil, fmt.Errorf("invalid bootstrap receiver identity")
		}
		return key, fileTunnelAddresses(doc), nil
	}
	raw, err := os.ReadFile(cfg.privateKeyFile)
	if err != nil {
		return wgtypes.Key{}, nil, err
	}
	key, err := wgtypes.ParseKey(strings.TrimSpace(string(raw)))
	if err != nil {
		return wgtypes.Key{}, nil, fmt.Errorf("invalid local receiver identity")
	}
	return key, nil, nil
}
