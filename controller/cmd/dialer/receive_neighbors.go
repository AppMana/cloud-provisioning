package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type receiveNeighborNative func(context.Context, wgtypes.Key, []tunnel.PeerSpec, []string, []tunnel.PeerSpec, []string, func() error) error

type receiveNeighborPreparation func(context.Context, wgtypes.Key, *corev1.Secret, []tunnel.PeerSpec, []tunnel.ReceiveTransitionPlan, func() error) error

type receiveNeighborCheckpoint struct {
	Version      int               `json:"version"`
	InventoryUID string            `json:"inventoryUID"`
	MeshUID      string            `json:"meshUID"`
	MeshVersion  string            `json:"meshVersion"`
	NodeUID      string            `json:"nodeUID"`
	BootID       string            `json:"bootID"`
	PublicKey    string            `json:"publicKey"`
	Peers        []tunnel.PeerSpec `json:"peers"`
	Retiring     []tunnel.PeerSpec `json:"retiring,omitempty"`
	// Candidates is used only in the retirement sidecar while native growth
	// is unresolved; the rollback-readable main checkpoint stays unchanged.
	Candidates []tunnel.PeerSpec `json:"candidates,omitempty"`
}

// Ordinary peers have independent forwarding authority. This operation keeps
// every journal and local phase fence, and never issues a phase receipt or a
// whole-mesh readiness acknowledgement for its partial work.
func reconcileReceiveNeighbors(ctx context.Context, api kubernetes.Interface, cfg config, native receiveNeighborNative, prepare receiveNeighborPreparation) error {
	cloud := cfg.secretName == ""
	namespace := cfg.secretNamespace
	if cloud {
		namespace = cfg.peersSecretNamespace
	}
	if api == nil || native == nil || namespace == "" || cfg.receiptMeshName == "" || (!cloud && cfg.secretName != cfg.receiptMeshName) || (cloud && cfg.peersFile == "") || cfg.receiptPodName == "" || cfg.receiptPodUID == "" {
		return fmt.Errorf("neighbor admission requires scoped node and agent identities")
	}
	lock, err := lockReconciliation(cfg)
	if err != nil {
		return err
	}
	defer lock.Close()
	key, allocation, err := receiveDispatchIdentity(cfg)
	if err != nil {
		return err
	}
	mesh, err := api.CoreV1().Secrets(namespace).Get(ctx, cfg.receiptMeshName, metav1.GetOptions{})
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
	store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps(namespace), ClusterUID: string(cluster.UID)}
	inventory, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return err
	}
	scope := receiveJournalScope{Namespace: namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID)}
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
		site, siteMember := plan.Sites[key.PublicKey().String()]
		if (cloud && plan.CloudNodeName == cfg.nodeName && plan.RootPublicKey == key.PublicKey().String()) || (!cloud && siteMember && site.NodeName == cfg.nodeName) {
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
	local := set.Local[scope.MachineUID]
	var peers []tunnel.PeerSpec
	plans := make([]tunnel.ReceiveTransitionPlan, 0, len(set.Routes.Plans))
	for _, uid := range set.Inventory.MachineUIDs {
		plans = append(plans, set.Routes.Plans[uid])
	}
	if cloud {
		if !slices.Equal(allocation, local.Plan.CloudAddresses) || len(allocation) == 0 {
			return fmt.Errorf("neighbor cloud allocation differs from committed plan")
		}
		address, err := netip.ParsePrefix(allocation[0])
		if err != nil {
			return err
		}
		projected, err := tunnel.RemotePeers(mesh.Data, address.Addr().String(), nil)
		if err != nil {
			return err
		}
		peers = make([]tunnel.PeerSpec, 0)
		for _, peer := range projected {
			if peer.Remote {
				peers = append(peers, peer)
			}
		}
	} else {
		role, _, err := tunnel.SiteForwardingRole(mesh.Data, cfg.nodeName, nil)
		if err != nil {
			return err
		}
		if role != "direct" {
			return fmt.Errorf("ordinary peer admission requires a direct site projection")
		}
		peers, err = tunnel.SitePeers(mesh.Data)
		if err != nil {
			return err
		}
	}
	if cloud {
		peers, err = tunnel.PlanReceiveCloudNeighborPeers(peers, plans, key.PublicKey().String())
	} else {
		peers, err = tunnel.PlanReceiveNeighborPeers(peers, plans)
	}
	if err != nil {
		return err
	}
	checkAgent := func() (string, error) {
		pod, err := api.CoreV1().Pods(namespace).Get(ctx, cfg.receiptPodName, metav1.GetOptions{})
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
		current, err := api.CoreV1().Secrets(namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
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
	var previousPeers []tunnel.PeerSpec
	legacy := true
	known := map[string]bool{}
	for _, source := range []string{path, path + ".retiring"} {
		raw, err := os.ReadFile(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		previous, err := decodeReceiveNeighborCheckpoint(raw, saved, source == path)
		if err != nil {
			return err
		}
		prior := previous.Peers
		if source != path {
			legacy = false
			prior = append(slices.Clone(previous.Retiring), previous.Candidates...)
		}
		if err := validateReceiveNeighborSourceGrowth(prior, peers); err != nil {
			return err
		}
		for _, peer := range prior {
			if identity := neighborPermissionIdentity(peer); !known[identity] {
				previousPeers = append(previousPeers, peer)
				known[identity] = true
			}
		}
	}
	var legacyCloudPath string
	var legacyCloud *receiveCloudLegacyImport
	if cloud {
		old, markerPath, marker, err := pendingReceiveCloudLegacy(path, saved, local.Plan, plans)
		if err != nil {
			return err
		}
		if err := validateReceiveNeighborSourceGrowth(old, peers); err != nil {
			return err
		}
		for _, peer := range old {
			if identity := neighborPermissionIdentity(peer); !known[identity] {
				previousPeers = append(previousPeers, peer)
				known[identity] = true
			}
		}
		legacyCloudPath, legacyCloud = markerPath, marker
	}
	// The old checkpoint recorded only the latest attempted publication. On
	// migration, the identity-bound legacy forwarding cache can recover peers
	// omitted by that attempt. It cannot authorize mutation or replay old routes.
	if legacy && !cloud {
		cached, err := readSiteForwardingCache(cfg, key)
		if err == nil {
			if cached.MeshUID != saved.MeshUID {
				return fmt.Errorf("neighbor forwarding cache mesh identity changed")
			}
			old, err := tunnel.SitePeers(cached.Data)
			if err != nil {
				return err
			}
			if err := validateReceiveNeighborSourceGrowth(old, peers); err != nil {
				return err
			}
			known := map[string]bool{}
			for _, peer := range previousPeers {
				known[neighborPermissionIdentity(peer)] = true
			}
			for _, peer := range old {
				if identity := neighborPermissionIdentity(peer); !known[identity] {
					previousPeers = append(previousPeers, peer)
					known[identity] = true
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	retirement := saved
	// Native installation may fail before or after changing source permissions.
	// Keep each exact admitted possibility until successful native readback.
	wanted := map[string]bool{}
	for _, peer := range peers {
		wanted[peer.PublicKey] = true
	}
	candidates := map[string]bool{}
	for _, peer := range append(slices.Clone(previousPeers), peers...) {
		identity := neighborPermissionIdentity(peer)
		if wanted[peer.PublicKey] && !candidates[identity] {
			retirement.Candidates = append(retirement.Candidates, peer)
			candidates[identity] = true
		}
	}
	if cloud {
		retirement.Retiring, err = tunnel.PlanReceiveCloudNeighborRetirements(previousPeers, peers, plans, key.PublicKey().String())
	} else {
		retirement.Retiring, err = tunnel.PlanReceiveNeighborRetirements(previousPeers, peers, plans)
	}
	if err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	// Keep retirement intent separate so a rollback binary can still read the
	// unchanged v1 neighbor checkpoint. Persist it first: replacing the main
	// checkpoint must never erase the only record of a withdrawn owner.
	if err := writeDurableJSON(path+".retiring", retirement); err != nil {
		return err
	}
	if err := writeDurableJSON(path, saved); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	// Import completion means the exact old permissions are durably represented
	// in the retirement sidecar, not that native deletion has succeeded. Record
	// it before native work: replaying the base after deletion and installation
	// could otherwise try to retire an address now owned by its replacement.
	if legacyCloud != nil {
		if err := writeDurableJSON(legacyCloudPath, legacyCloud); err != nil {
			return err
		}
	}
	var allocated []string
	var own []string
	if cloud {
		allocated = slices.Clone(local.Plan.CloudAddresses)
	} else {
		own = tunnel.ReceiveSiteRouteSources(mesh.Data, cfg.nodeName)
	}
	if site := local.Plan.Sites[key.PublicKey().String()]; site.Legacy != nil && site.Legacy.Tunnel != nil {
		allocated = site.Legacy.Tunnel.Addresses
	}
	commitRetirement := func() error {
		if err := check(); err != nil {
			return err
		}
		retirement.Retiring = nil
		return writeDurableJSON(path+".retiring", retirement)
	}
	if err := native(ctx, key, peers, own, retirement.Retiring, allocated, commitRetirement); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	// Successful native readback resolves installation ambiguity even if the
	// separately prepared receiver later fails. Do not retain obsolete variants
	// as deletion authority after confirming the new permissions.
	retirement.Retiring = nil
	retirement.Candidates = nil
	if err := writeDurableJSON(path+".retiring", retirement); err != nil {
		return err
	}
	if prepare != nil && !cloud {
		if err := prepare(ctx, key, mesh, peers, plans, check); err != nil {
			return err
		}
	}
	return check()
}

// Native ordinary-peer reconciliation accepts growth only. Reject unsupported
// withdrawal before replacing durable intent, including after a failed attempt:
// later whole-peer retirement still needs the exact previous permissions.
func validateReceiveNeighborSourceGrowth(previous, desired []tunnel.PeerSpec) error {
	wanted := make(map[string]map[string]bool, len(desired))
	for _, peer := range desired {
		sources := make(map[string]bool, len(peer.WGAllowedIPs))
		for _, source := range peer.WGAllowedIPs {
			sources[source] = true
		}
		wanted[peer.PublicKey] = sources
	}
	for _, peer := range previous {
		sources, retained := wanted[peer.PublicKey]
		if !retained {
			continue
		}
		for _, source := range peer.WGAllowedIPs {
			if !sources[source] {
				return fmt.Errorf("withdrawing existing neighbor source authorization requires a separate transition")
			}
		}
	}
	return nil
}

// Endpoints may roam; retirement authority is the exact key and source set.
func neighborPermissionIdentity(peer tunnel.PeerSpec) string {
	sources := slices.Clone(peer.WGAllowedIPs)
	slices.Sort(sources)
	raw, _ := json.Marshal(sources)
	return peer.PublicKey + string(raw)
}

// Earlier boots may supply ownership intent, but never live execution authority.
func decodeReceiveNeighborCheckpoint(raw []byte, expected receiveNeighborCheckpoint, main bool) (receiveNeighborCheckpoint, error) {
	var previous receiveNeighborCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&previous); err != nil {
		return previous, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return previous, fmt.Errorf("trailing neighbor checkpoint data")
	}
	if previous.Version != 1 || previous.InventoryUID != expected.InventoryUID || previous.MeshUID != expected.MeshUID || previous.NodeUID != expected.NodeUID || previous.BootID == "" || previous.PublicKey != expected.PublicKey || previous.Peers == nil || (main && (len(previous.Retiring) != 0 || len(previous.Candidates) != 0)) {
		return previous, fmt.Errorf("neighbor checkpoint identity changed")
	}
	return previous, nil
}
