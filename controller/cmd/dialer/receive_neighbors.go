package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type receiveNeighborNative func(context.Context, wgtypes.Key, []tunnel.PeerSpec, []string) error

type receiveNeighborPreparation func(context.Context, wgtypes.Key, *corev1.Secret, []tunnel.PeerSpec, func() error) error

type receiveNeighborCheckpoint struct {
	Version      int               `json:"version"`
	InventoryUID string            `json:"inventoryUID"`
	MeshUID      string            `json:"meshUID"`
	MeshVersion  string            `json:"meshVersion"`
	NodeUID      string            `json:"nodeUID"`
	BootID       string            `json:"bootID"`
	PublicKey    string            `json:"publicKey"`
	Peers        []tunnel.PeerSpec `json:"peers"`
}

// Ordinary peers have independent forwarding authority. This operation keeps
// every journal and local phase fence, and never issues a phase receipt or a
// whole-mesh readiness acknowledgement for its partial work.
func reconcileReceiveNeighbors(ctx context.Context, api kubernetes.Interface, cfg config, native receiveNeighborNative, prepare receiveNeighborPreparation) error {
	if cfg.secretName == "" {
		return nil
	} // Cloud documents need their own projection.
	if api == nil || native == nil || cfg.secretNamespace == "" || cfg.secretName != cfg.receiptMeshName || cfg.receiptPodName == "" || cfg.receiptPodUID == "" {
		return fmt.Errorf("neighbor admission requires scoped site and agent identities")
	}
	lock, err := lockReconciliation(cfg)
	if err != nil {
		return err
	}
	defer lock.Close()
	key, _, err := receiveDispatchIdentity(cfg)
	if err != nil {
		return err
	}
	mesh, err := api.CoreV1().Secrets(cfg.secretNamespace).Get(ctx, cfg.secretName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if mesh.UID == "" || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil {
		return fmt.Errorf("neighbor mesh identity missing")
	}
	cluster, err := api.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cluster.UID == "" || cluster.DeletionTimestamp != nil {
		return fmt.Errorf("neighbor cluster identity missing")
	}
	store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps(cfg.secretNamespace), ClusterUID: string(cluster.UID)}
	inventory, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, cfg.secretNamespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return err
	}
	scope := receiveJournalScope{Namespace: cfg.secretNamespace, MeshName: mesh.Name, MeshUID: string(mesh.UID)}
	for _, uid := range inventory.MachineUIDs {
		store.Name = tunnel.ReceiveTransitionName(uid)
		snapshot, err := store.Load(ctx)
		if err != nil {
			return err
		}
		plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
		if err != nil {
			return err
		}
		if site, ok := plan.Sites[key.PublicKey().String()]; ok && site.NodeName == cfg.nodeName {
			scope.MachineUID = uid
			break
		}
	}
	if scope.MachineUID == "" {
		return fmt.Errorf("neighbor writer has no local reserved journal")
	}
	store.Name = tunnel.ReceiveTransitionName(scope.MachineUID)
	set, err := loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key)
	if err != nil {
		return err
	}
	if err := checkpointReceiveSetLocked(ctx, api, store, scope, cfg, key, lock.Name(), set); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for _, local := range set.Local {
		view, err := local.Snapshot.Transition().View()
		if err != nil {
			return err
		}
		if !view.Complete {
			view, err = executableReceiveRound(local.Snapshot, time.Now())
			if err != nil {
				return err
			}
			if view.Round.ExpiresAt.Before(deadline) {
				deadline = view.Round.ExpiresAt
			}
		}
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	role, _, err := tunnel.SiteForwardingRole(mesh.Data, cfg.nodeName, nil)
	if err != nil {
		return err
	}
	if role != "direct" {
		return fmt.Errorf("ordinary peer admission requires a direct site projection")
	}
	peers, err := tunnel.SitePeers(mesh.Data)
	if err != nil {
		return err
	}
	plans := make([]tunnel.ReceiveTransitionPlan, 0, len(set.Routes.Plans))
	for _, uid := range set.Inventory.MachineUIDs {
		plans = append(plans, set.Routes.Plans[uid])
	}
	peers, err = tunnel.PlanReceiveNeighborPeers(peers, plans)
	if err != nil {
		return err
	}
	local := set.Local[scope.MachineUID]
	checkAgent := func() (string, error) {
		pod, err := api.CoreV1().Pods(cfg.secretNamespace).Get(ctx, cfg.receiptPodName, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		if string(pod.UID) != cfg.receiptPodUID || pod.Spec.NodeName != cfg.nodeName || pod.DeletionTimestamp != nil {
			return "", fmt.Errorf("neighbor agent incarnation changed")
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "dialer" && status.Ready && status.State.Running != nil && status.ContainerID != "" {
				return status.ContainerID, nil
			}
		}
		return "", fmt.Errorf("neighbor agent is not ready")
	}
	container, err := checkAgent()
	if err != nil {
		return err
	}
	check := func() error {
		currentCluster, err := api.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if currentCluster.UID != cluster.UID || currentCluster.DeletionTimestamp != nil {
			return fmt.Errorf("neighbor cluster identity changed")
		}
		current, err := api.CoreV1().Secrets(cfg.secretNamespace).Get(ctx, mesh.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !sameReceiveRevision(mesh.ObjectMeta, current.ObjectMeta) {
			return fmt.Errorf("neighbor mesh publication changed")
		}
		currentSet, err := loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key)
		if err != nil {
			return err
		}
		if !sameReceiveSet(set, currentSet) {
			return fmt.Errorf("neighbor journal inventory changed")
		}
		currentContainer, err := checkAgent()
		if err != nil {
			return err
		}
		if currentContainer != container {
			return fmt.Errorf("neighbor agent container changed")
		}
		return ctx.Err()
	}
	// Earlier-boot intent is admissible only after the complete live journal
	// set has verified the current local boot. Cached peers are never replayed.
	saved := receiveNeighborCheckpoint{Version: 1, InventoryUID: string(set.Metadata.UID), MeshUID: string(mesh.UID), MeshVersion: mesh.ResourceVersion, NodeUID: local.NodeUID, BootID: local.BootID, PublicKey: key.PublicKey().String(), Peers: peers}
	path := lock.Name() + ".neighbors.json"
	if raw, err := os.ReadFile(path); err == nil {
		var previous receiveNeighborCheckpoint
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&previous); err != nil {
			return err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return fmt.Errorf("trailing neighbor checkpoint data")
		}
		if previous.Version != 1 || previous.InventoryUID != saved.InventoryUID || previous.MeshUID != saved.MeshUID || previous.NodeUID != saved.NodeUID || previous.BootID == "" || previous.PublicKey != saved.PublicKey || previous.Peers == nil {
			return fmt.Errorf("neighbor checkpoint identity changed")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	if err := writeDurableJSON(path, saved); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	if err := native(ctx, key, peers, tunnel.ReceiveSiteRouteSources(mesh.Data, cfg.nodeName)); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	if prepare != nil {
		if err := prepare(ctx, key, mesh, peers, check); err != nil {
			return err
		}
	}
	return check()
}
