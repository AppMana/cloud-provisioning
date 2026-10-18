package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Retirement backends must verify every local cleanup obligation. No backend
// means no retirement receipt; phase maintenance is never a fallback.
type receiveDispatchRetirement func(context.Context, handover.Store, receiveJournalScope, wgtypes.Key, string, admittedReceiveRetirement) error

type receiveDispatchNative func(context.Context, tunnel.ReceiveTransitionPlan, handover.View, string, tunnel.ReceiveJournalRoutes, wgtypes.Key) error

// A local reserved journal owns forwarding even after completion. Never fall
// through to legacy reconciliation on an API, credential, admission or native
// failure. Releasing reservations and restoring ordinary publishers is separate.
func dispatchReceivePhases(ctx context.Context, api kubernetes.Interface, cfg config, native, maintain receiveDispatchNative, retirement ...receiveDispatchRetirement) (handled bool, resultErr error) {
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
	retiring := map[string]bool{}
	for _, uid := range inventory.MachineUIDs {
		store.Name = tunnel.ReceiveTransitionName(uid)
		snapshot, err := store.Load(ctx)
		if err != nil {
			return true, err
		}
		if err := inventory.CheckJournal(uid, string(snapshot.Metadata().UID)); err != nil {
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
		if len(inventory.Publications[uid].Retirement) != 0 {
			withdrawal, err := attachment.ReadReceiveWithdrawal(inventory, uid, snapshot, mesh)
			if err != nil {
				return true, err
			}
			// Retained followers and surviving cloud workers may not be participants
			// in this worker's receiver journal. Persisted consumers own this decision.
			local = false
			for _, consumer := range withdrawal.Consumers {
				local = local || consumer.NodeName == cfg.nodeName
			}
			retiring[uid] = true
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
	// Retained-consumer observations have a short shared freshness window.
	// Do not interleave other receiver maintenance between withdrawal targets
	// merely because their opaque machine UIDs sort between those targets.
	// Preserve discovery order within each group and all admission checks.
	slices.SortStableFunc(targets, func(a, b string) int {
		if retiring[a] == retiring[b] {
			return 0
		}
		if retiring[a] {
			return -1
		}
		return 1
	})
	var failures []error
	var deliveries receiveRetirementDeliveries
	// A later discovery or identity read must not hide failures already
	// observed for earlier receivers in this reconciliation pass.
	// Join outstanding deliveries before the deferred transport.Close, even
	// when a later target fails admission. One failed request cannot cancel the
	// other consumers' observations or escape this reconciliation pass.
	defer func() { resultErr = errors.Join(append(failures, resultErr, deliveries.wait())...) }()
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
		if retiring[uid] {
			if len(retirement) != 1 || retirement[0] == nil {
				failures = append(failures, fmt.Errorf("retirement requires a complete native cleanup backend"))
				continue
			}
			receipts, err := executeReceiveRetirement(ctx, api, store, scope, cfg, key, containerID, func(ctx context.Context, lockPath string, admitted admittedReceiveRetirement) error {
				return retirement[0](ctx, store, scope, key, lockPath, admitted)
			})
			if err != nil {
				failures = append(failures, fmt.Errorf("receiver %s retirement execution: %w", uid, err))
				continue
			}
			deliveries.submit(func() error {
				delivery := newReceiveTiming(true, "retirement-delivery")
				delivery.step("request")
				err := transport.Submit(ctx, strings.TrimSpace(string(token)), handover.ReceiptRequest{MachineUID: uid, RoundHash: receipts[0].Receipt.RoundHash, Receipts: receipts})
				delivery.finish(err == nil)
				if err != nil {
					return fmt.Errorf("receiver %s retirement delivery: %w", uid, err)
				}
				return nil
			})
			continue
		}
		snapshot, err := store.Load(ctx)
		if err != nil {
			return true, err
		}
		view, err := snapshot.Transition().View()
		if err != nil {
			return true, err
		}
		wrap := func(action receiveDispatchNative) receiveNativePhase {
			return func(ctx context.Context, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes) error {
				_, expected, err := tunnel.ReceiveLocalNetwork(plan, nodeUID)
				if err != nil {
					return err
				}
				if cfg.peersFile != "" && !slices.Equal(allocated, expected) {
					return fmt.Errorf("bootstrap tunnel allocation differs from committed plan")
				}
				return action(ctx, plan, view, nodeUID, intent, key)
			}
		}
		verify := wrap(native)
		if view.Complete {
			// Restore and verify under one admission and host lock so no other
			// reconciler can interleave with selected-transport restoration.
			completed := verify
			if maintain != nil {
				completed = func(ctx context.Context, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes) error {
					if err := wrap(maintain)(ctx, plan, view, nodeUID, intent); err != nil {
						return err
					}
					return verify(ctx, plan, view, nodeUID, intent)
				}
			}
			if err := maintainCompletedReceivePhase(ctx, api, store, scope, cfg, key, containerID, completed); err != nil {
				failures = append(failures, fmt.Errorf("receiver %s completed maintenance: %w", uid, err))
			}
			continue
		}
		receipts, err := executeReceivePhase(ctx, api, store, scope, cfg, key, containerID, verify)
		if err != nil {
			failures = append(failures, fmt.Errorf("receiver %s phase execution: %w", uid, err))
			continue
		}
		delivery := newReceiveTiming(true, "phase-delivery")
		delivery.step("request")
		deliveryErr := transport.Submit(ctx, strings.TrimSpace(string(token)), handover.ReceiptRequest{MachineUID: uid, RoundHash: receipts[0].Receipt.RoundHash, Receipts: receipts})
		delivery.finish(deliveryErr == nil)
		if err := deliveryErr; err != nil {
			failures = append(failures, fmt.Errorf("receiver %s phase delivery: %w", uid, err))
		}
	}
	return true, nil
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
