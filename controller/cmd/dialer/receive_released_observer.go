package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Caller holds the host lock. An unpublished site observer has no ordinary
// routing authority, but must still advance its durable released-owner fences.
// Admit every active history and reject local ownership before saving anything.
// This never invokes native work, issues receipts or removes local journals.
func checkpointReleasedReceiveObserver(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, lockPath string, mesh *corev1.Secret) error {
	return withReleasedReceiveObserver(ctx, api, store, scope, cfg, key, lockPath, mesh, nil)
}

type receiveObserverAdmission struct {
	Inventory tunnel.ReceiveInventory
	Metadata  metav1.ObjectMeta
	Journals  map[string]handover.Snapshot
	Mesh      *corev1.Secret
	Node      *corev1.Node
}

// Caller holds the host lock. The callback receives authenticated observer
// identity and histories, not unrestricted routing authority. It must separately
// admit source ownership, relay dependencies and durable native ownership, and
// call check before mutations. Both boundaries retain the exact discovery and
// participant identities; no callback may issue a receiver-phase receipt.
func withReleasedReceiveObserver(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, lockPath string, mesh *corev1.Secret, use func(context.Context, receiveObserverAdmission, func() error) error) error {
	if use != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	if cfg.secretName == "" || mesh == nil || scope.MachineUID != "" || len(mesh.Data[tunnel.NodePublicKeyPrefix+cfg.nodeName]) != 0 || len(mesh.Data[tunnel.NodeTunnelAddressPrefix+cfg.nodeName]) != 0 || len(mesh.Data[tunnel.NodeTunnelAddress6Prefix+cfg.nodeName]) != 0 {
		return fmt.Errorf("released observer requires unpublished site identity")
	}
	if _, err := readPrivateRetirementCache(lockPath + ".inventory.json"); err != nil {
		return err
	}
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	if err != nil {
		return err
	}
	if len(inventory.Released) == 0 {
		return fmt.Errorf("released observer requires retained release history")
	}
	node, err := api.CoreV1().Nodes().Get(ctx, cfg.nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	active := map[string]handover.Snapshot{}
	routes := tunnel.ReceiveJournalRoutes{Plans: map[string]tunnel.ReceiveTransitionPlan{}}
	var plans []tunnel.ReceiveTransitionPlan
	for _, uid := range inventory.MachineUIDs {
		member := store
		member.Name, member.ExpectedUID = tunnel.ReceiveTransitionName(uid), inventory.Publications[uid].JournalUID
		snapshot, err := member.Load(ctx)
		if err != nil {
			return err
		}
		if err := inventory.CheckJournal(uid, string(snapshot.Metadata().UID)); err != nil {
			return err
		}
		if _, err := member.ArchiveNames(ctx, snapshot); err != nil {
			return err
		}
		if err := tunnel.ValidateReceiveTransportHistory(ctx, member, snapshot); err != nil {
			return err
		}
		plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
		if err != nil {
			return err
		}
		if plan.MachineUID != uid || plan.Namespace != scope.Namespace || plan.MeshName != scope.MeshName || plan.MeshUID != scope.MeshUID {
			return fmt.Errorf("observer active journal scope changed")
		}
		if plan.CloudNodeName == cfg.nodeName || plan.CloudNodeUID == string(node.UID) || plan.RootPublicKey == key.PublicKey().String() {
			return fmt.Errorf("observer has active receiver ownership")
		}
		for public, site := range plan.Sites {
			if site.NodeName == cfg.nodeName || site.NodeUID == string(node.UID) || public == key.PublicKey().String() {
				return fmt.Errorf("observer has active site ownership")
			}
		}
		if len(inventory.Publications[uid].Retirement) != 0 {
			withdrawal, err := attachment.ReadReceiveWithdrawal(inventory, uid, snapshot, mesh)
			if err != nil {
				return err
			}
			for _, consumer := range withdrawal.Consumers {
				if consumer.NodeName == cfg.nodeName || consumer.NodeUID == string(node.UID) || consumer.PublicKey == key.PublicKey().String() {
					return fmt.Errorf("observer still requires native retirement execution")
				}
			}
		}
		active[uid], plans = snapshot, append(plans, plan)
		routes.Plans[uid] = plan
	}
	if len(plans) != 0 {
		if _, err := tunnel.PlanReceiveRouteSet(plans); err != nil {
			return err
		}
	}
	if err := appendReleasedReceiveExclusions(inventory, &routes); err != nil {
		return err
	}
	boot, err := localReceiveBootID()
	if err != nil {
		return err
	}
	next := receiveInventoryCheckpoint{Version: 1, InventoryUID: string(index.UID), Inventory: inventory, NodeName: cfg.nodeName, NodeUID: string(node.UID), BootID: boot, PublicKey: key.PublicKey().String()}
	proof, err := admitReleasedReceiveCheckpoints(ctx, api, store, scope, cfg, key, lockPath, next, index.ObjectMeta)
	if err != nil {
		return err
	}
	if err := validateReceiveInventoryCheckpointWithRelease(lockPath+".inventory.json", next, proof); err != nil {
		return err
	}
	neighbors, err := readReceiveBootNeighborFiles(lockPath, next, routes.Withdrawn)
	if err != nil {
		return err
	}
	for name, raw := range neighbors {
		expected := receiveNeighborCheckpoint{InventoryUID: next.InventoryUID, MeshUID: scope.MeshUID, NodeUID: next.NodeUID, PublicKey: next.PublicKey}
		saved, err := decodeReceiveNeighborCheckpoint(raw, expected, name == lockPath+".neighbors.json")
		if err != nil {
			return err
		}
		if len(saved.Peers) != 0 {
			return fmt.Errorf("unpublished observer retains ordinary neighbor ownership")
		}
	}
	entries, err := os.ReadDir(filepath.Dir(lockPath))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(lockPath)+".receive-transition-") && !proof.localFiles[entry.Name()] {
			return fmt.Errorf("observer omitted a checkpointed local journal")
		}
	}
	pod, err := api.CoreV1().Pods(scope.Namespace).Get(ctx, cfg.receiptPodName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	receipt := handover.Receipt{NodeUID: next.NodeUID, ObservedBootID: boot, AgentPodName: cfg.receiptPodName, AgentPodUID: cfg.receiptPodUID, AgentContainerName: "dialer", ObservedAt: time.Now()}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "dialer" && status.Ready && status.State.Running != nil {
			receipt.AgentContainerID = status.ContainerID
		}
	}
	if err := handover.VerifyObservationIncarnation(ctx, api, scope.Namespace, cfg.nodeName, receipt); err != nil {
		return err
	}
	check := func() error {
		cluster, err := api.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if string(cluster.UID) != store.ClusterUID || cluster.DeletionTimestamp != nil {
			return fmt.Errorf("observer cluster identity changed")
		}
		for uid, before := range active {
			member := store
			member.Name, member.ExpectedUID = tunnel.ReceiveTransitionName(uid), string(before.Metadata().UID)
			current, err := member.Load(ctx)
			if err != nil {
				return err
			}
			if err := current.ContinuesExecution(before); err != nil {
				return err
			}
		}
		_, currentIndex, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
		if err != nil {
			return err
		}
		if !sameReceiveRevision(index.ObjectMeta, currentIndex.ObjectMeta) {
			return fmt.Errorf("observer inventory changed during admission")
		}
		if err := proof.recheck(ctx, api, store); err != nil {
			return err
		}
		currentMesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !sameReceiveRevision(mesh.ObjectMeta, currentMesh.ObjectMeta) {
			return fmt.Errorf("observer mesh changed during checkpoint admission")
		}
		currentNode, err := api.CoreV1().Nodes().Get(ctx, cfg.nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !sameReceiveParticipantNode(node, currentNode) {
			return fmt.Errorf("observer source allocation or identity changed")
		}
		if err := handover.VerifyObservationIncarnation(ctx, api, scope.Namespace, cfg.nodeName, receipt); err != nil {
			return err
		}
		return ctx.Err()
	}
	if err := check(); err != nil {
		return err
	}
	if err := writeDurableJSON(lockPath+".inventory.json", next); err != nil {
		return err
	}
	if use == nil {
		return nil
	}
	if err := use(ctx, receiveObserverAdmission{Inventory: inventory, Metadata: index.ObjectMeta, Journals: active, Mesh: mesh, Node: node}, check); err != nil {
		return err
	}
	return check()
}
