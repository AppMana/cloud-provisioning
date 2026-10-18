package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// A surviving cloud worker removes only the departed cloud neighbor. Its own
// receiver transport, stable allocation and site peers remain owned by its local
// journal. Caller holds the host lock; completion still needs executor rechecks.
func retireAdmittedCloudNeighborLocked(ctx context.Context, api kubernetes.Interface, devices receiverDevices, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, lockPath string, admitted admittedReceiveRetirement) error {
	if devices == nil || cfg.secretName != "" || cfg.peersFile == "" || cfg.peersSecretNamespace != scope.Namespace || cfg.receiptMeshName != scope.MeshName {
		return fmt.Errorf("cloud retirement requires matching bootstrap scope")
	}
	var adoption *corev1.Secret
	check := func() error {
		current, err := loadReceiveRetirement(ctx, api, store, scope, cfg.nodeName, key)
		if err != nil {
			return err
		}
		if !sameReceiveRetirement(admitted, current) {
			return fmt.Errorf("cloud retirement authority changed")
		}
		if adoption != nil {
			name, err := cloudAdoptionSecretName(cfg)
			if err != nil {
				return err
			}
			if name != adoption.Name {
				return fmt.Errorf("cloud adoption source changed during retirement")
			}
			current, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, adoption.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if !sameReceiveRevision(adoption.ObjectMeta, current.ObjectMeta) {
				return fmt.Errorf("surviving cloud adoption changed during retirement")
			}
		}
		return ctx.Err()
	}
	if err := check(); err != nil {
		return err
	}
	var local *tunnel.ReceiveTransitionPlan
	for i := range admitted.Plans {
		plan := &admitted.Plans[i]
		if plan.CloudNodeUID == admitted.NodeUID && plan.RootPublicKey == key.PublicKey().String() && plan.CloudNodeName == cfg.nodeName {
			if local != nil {
				return fmt.Errorf("ambiguous local cloud retirement identity")
			}
			local = plan
		}
	}
	if local == nil || local.MachineUID == scope.MachineUID {
		return fmt.Errorf("cloud retirement requires a surviving local journal")
	}
	if err := validateCloudRetirementConsumer(cfg, *local, admitted); err != nil {
		return err
	}
	own, allocated, err := tunnel.ReceiveLocalNetwork(*local, admitted.NodeUID)
	if err != nil {
		return err
	}
	actualKey, actualAllocation, err := receiveDispatchIdentity(cfg)
	if err != nil {
		return err
	}
	if actualKey != key || !slices.Equal(allocated, actualAllocation) {
		return fmt.Errorf("cloud retirement allocation changed")
	}
	target, err := tunnel.DecodeReceiveTransition(admitted.Journals[scope.MachineUID], store.ClusterUID)
	if err != nil {
		return err
	}
	adoption, err = api.CoreV1().Secrets(scope.Namespace).Get(ctx, local.AdoptionName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(adoption.UID) != local.AdoptionUID || adoption.DeletionTimestamp != nil || adoption.ResourceVersion == "" {
		return fmt.Errorf("surviving cloud adoption identity changed")
	}
	var published tunnel.PeerListDoc
	if err := json.Unmarshal(adoption.Data[tunnel.CloudPeersKey], &published); err != nil {
		return err
	}
	if published.Peers == nil {
		return fmt.Errorf("surviving cloud peer publication is incomplete")
	}
	for _, peer := range published.Peers {
		if peer.PublicKey == target.RootPublicKey {
			return fmt.Errorf("withdrawn neighbor remains in surviving cloud publication")
		}
	}
	caches, err := prepareWithdrawnCloudCaches(cfg, key, allocated, target.RootPublicKey)
	if err != nil {
		return err
	}
	var recorded []tunnel.PeerSpec
	for _, cache := range caches {
		recorded = append(recorded, cache.peers...)
	}
	for _, plan := range []tunnel.ReceiveTransitionPlan{*local, target} {
		var base tunnel.PeerListDoc
		if err := json.Unmarshal(plan.Base, &base); err != nil {
			return err
		}
		recorded = append(recorded, base.Peers...)
		recorded = append(recorded, plan.Desired.Peers...)
		recorded = append(recorded, plan.SiteDesired.Peers...)
	}
	address, err := netip.ParsePrefix(allocated[0])
	if err != nil {
		return err
	}
	projected, err := tunnel.RemotePeers(admitted.Mesh.Data, address.Addr().String(), nil)
	if err != nil {
		return err
	}
	current := make([]tunnel.PeerSpec, 0)
	for _, peer := range projected {
		if peer.Remote {
			current = append(current, peer)
		}
	}
	if err := checkpointReceiveRetirementLocked(ctx, api, store, scope, cfg, key, lockPath, admitted); err != nil {
		return err
	}
	checkpoint := receiveNeighborCheckpoint{Version: 1, InventoryUID: string(admitted.Metadata.UID), MeshUID: scope.MeshUID, MeshVersion: admitted.MeshMetadata.ResourceVersion, NodeUID: admitted.NodeUID, BootID: admitted.BootID, PublicKey: key.PublicKey().String(), Peers: current}
	if err := retireWithdrawnNeighborCaches(ctx, lockPath+".neighbors.json", checkpoint, admitted.Plans, target.RootPublicKey, recorded, check, func(peers []tunnel.PeerSpec) error {
		if err := retireReceiveNeighbors(ctx, devices, cfg, key, peers, own, allocated); err != nil {
			return err
		}
		live, err := devices.Device(cfg.iface)
		if err != nil {
			return err
		}
		for _, peer := range live.Peers {
			if peer.PublicKey.String() == target.RootPublicKey {
				return fmt.Errorf("withdrawn cloud neighbor remains installed")
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	return commitWithdrawnCloudCaches(caches)
}

// The retained consumer, its local journal and the ordinary reconciliation
// source must refer to the same worker. Matching a public key alone is not
// sufficient to authorize changing durable bootstrap state.
func validateCloudRetirementConsumer(cfg config, local tunnel.ReceiveTransitionPlan, admitted admittedReceiveRetirement) error {
	name, err := cloudAdoptionSecretName(cfg)
	if err != nil {
		return err
	}
	if name != local.AdoptionName {
		return fmt.Errorf("cloud retirement adoption source changed")
	}
	matches := 0
	for _, consumer := range admitted.Withdrawal.Consumers {
		if consumer.NodeUID != admitted.NodeUID {
			continue
		}
		matches++
		if consumer.Site || consumer.NodeName != cfg.nodeName || consumer.NodeName != local.CloudNodeName || consumer.NodeUID != local.CloudNodeUID || consumer.MachineName != local.MachineName || consumer.SecretUID != local.AdoptionUID || consumer.PublicKey != local.RootPublicKey {
			return fmt.Errorf("cloud retirement consumer identity changed")
		}
		owned := false
		for _, raw := range local.CloudAddresses {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				return err
			}
			owned = owned || prefix.Addr().String() == consumer.TunnelAddress
		}
		if !owned {
			return fmt.Errorf("cloud retirement consumer allocation changed")
		}
	}
	if matches != 1 {
		return fmt.Errorf("cloud retirement requires exactly one retained consumer")
	}
	return nil
}
