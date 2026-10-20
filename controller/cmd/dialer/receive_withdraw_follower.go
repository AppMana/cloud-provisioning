package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Caller holds the host lock. Follower-only cleanup uses recorded relay routes;
// former endpoints with receiver ownership remain a separate recovery case.
// The executor rechecks the agent incarnation before returning an observation.
func retireAdmittedFollowerLocked(ctx context.Context, api kubernetes.Interface, speaker *transitSpeaker, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, lockPath string, admitted admittedReceiveRetirement) error {
	if cfg.secretName != scope.MeshName || cfg.secretNamespace != scope.Namespace || siteForwardingCachePath(cfg) == "" {
		return fmt.Errorf("follower retirement requires matching mesh storage")
	}
	retained := false
	for _, c := range admitted.Withdrawal.Consumers {
		if c.NodeUID == admitted.NodeUID && c.NodeName == cfg.nodeName && c.Site {
			retained = true
		}
	}
	if !retained || strings.TrimSpace(string(admitted.Mesh.Data[tunnel.NodeTunnelAddressPrefix+cfg.nodeName])) != "" {
		return fmt.Errorf("retirement consumer is not a follower")
	}
	for _, plan := range admitted.Plans {
		if plan.CloudNodeUID == admitted.NodeUID {
			return fmt.Errorf("follower has cloud receiver obligations")
		}
		for _, site := range plan.Sites {
			if site.NodeUID == admitted.NodeUID {
				return fmt.Errorf("former endpoint requires receiver retirement")
			}
		}
	}
	node, err := api.CoreV1().Nodes().Get(ctx, cfg.nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var checkFollowerRecord func() error
	check := func() error {
		if err := recheckReceiveRetirement(ctx, api, store, admitted, cfg.nodeName, key); err != nil {
			return err
		}
		currentNode, err := api.CoreV1().Nodes().Get(ctx, cfg.nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !sameReceiveParticipantNode(node, currentNode) {
			return fmt.Errorf("follower node source changed")
		}
		if _, err := netlink.LinkByName(cfg.iface); !isLinkNotFound(err) {
			return fmt.Errorf("follower has unresolved local tunnel state: %v", err)
		}
		base := siteReceiveCachePath(cfg)
		for _, path := range []string{base, base + ".admitted", base + ".owners", base + ".admitted.owners", lockPath + ".neighbors.json", lockPath + ".neighbors.json.retiring"} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				return fmt.Errorf("follower has unresolved receiver cache state")
			}
		}
		if checkFollowerRecord != nil {
			if err := checkFollowerRecord(); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	if err := check(); err != nil {
		return err
	}
	target, err := tunnel.DecodeReceiveTransition(admitted.Journals[scope.MachineUID], store.ClusterUID)
	if err != nil {
		return err
	}
	forwarding, err := prepareWithdrawnTransitCache(cfg, key, scope.MeshUID, target.MachineName, target.RootPublicKey)
	if err != nil {
		return err
	}
	routes, err := planWithdrawnTransitRoutes(forwarding, admitted.Plans, target.RootPublicKey)
	if err != nil {
		return err
	}
	// The cache records historical ownership, while the admitted live mesh
	// protects newly published survivors that were absent from that cache.
	currentPeers, err := tunnel.SitePeers(admitted.Mesh.Data)
	if err != nil {
		return err
	}
	withdrawnPeers, err := tunnel.PlanReceiveWithdrawnNeighborPeers(withdrawnTransitPeerVariants(forwarding, admitted.Plans, target.RootPublicKey), currentPeers, admitted.Plans, target.RootPublicKey)
	if err != nil {
		return err
	}
	prefixes := map[string]bool{}
	for _, peer := range withdrawnPeers {
		for _, raw := range peer.WGAllowedIPs {
			prefixes[raw] = true
		}
		for _, host := range peer.AllRouteHosts() {
			prefixes[tunnel.HostCIDR(host)] = true
		}
	}
	var own []string
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP && a.Address != "" {
			own = append(own, a.Address)
		}
	}
	if !slices.Equal(own, forwarding.next.Own) {
		return fmt.Errorf("follower source allocation changed")
	}
	relayVariants := []map[string]string{routes}
	followerPath := lockPath + ".follower.json"
	followerScope := receiveInventoryCheckpoint{Version: 1, InventoryUID: string(admitted.Metadata.UID), Inventory: admitted.Inventory, NodeName: cfg.nodeName, NodeUID: admitted.NodeUID, BootID: admitted.BootID, PublicKey: key.PublicKey().String()}
	follower, err := readReceiveFollowerCache(followerPath, cfg, followerScope, own)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		checkFollowerRecord = func() error {
			current, err := readReceiveFollowerCache(followerPath, cfg, followerScope, own)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(current, follower) {
				return fmt.Errorf("follower route ownership changed during retirement")
			}
			return nil
		}
		for _, variant := range follower.Routes[target.RootPublicKey] {
			// Reuse the existing destination-ownership check. This record
			// describes routes only, so it grants no WireGuard permissions.
			peer := tunnel.PeerSpec{PublicKey: target.RootPublicKey, Remote: true}
			for raw := range variant {
				peer.WGAllowedIPs = append(peer.WGAllowedIPs, raw)
			}
			slices.Sort(peer.WGAllowedIPs)
			if _, err := tunnel.PlanReceiveWithdrawnNeighborPeers([]tunnel.PeerSpec{peer}, currentPeers, admitted.Plans, target.RootPublicKey); err != nil {
				return err
			}
			relayVariants = append(relayVariants, variant)
		}
	} else {
		checkFollowerRecord = func() error {
			if _, err := os.Lstat(followerPath); !os.IsNotExist(err) {
				return fmt.Errorf("follower route ownership appeared during retirement")
			}
			return nil
		}
	}
	advertisements, err := prepareWithdrawnAdvertisementCache(cfg, speaker, forwarding)
	if err != nil {
		return err
	}
	if err := checkpointReceiveRetirementLocked(ctx, api, store, scope, cfg, key, lockPath, admitted); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	for _, variant := range relayVariants {
		for raw := range variant {
			prefixes[raw] = true
		}
	}
	if speaker != nil {
		if err := speaker.withdrawReceivePrefixes(ctx, prefixes); err != nil {
			return err
		}
	}
	if err := withdrawReceiveDeviceRoutes(ctx, cfg, []map[string]receiveRouteSelection{{}}, own, relayVariants...); err != nil {
		return err
	}
	// Even an empty legacy snapshot or a retry after record compaction must
	// prove the committed destinations absent. No relay map here means that
	// any remaining scoped route or foreign more-specific route is rejected.
	absent := cfg
	absent.receiveRouteScope, absent.receiveRoutes, absent.receiveRelayRoutes = prefixes, nil, nil
	if err := preflightReceiveRouteGenerationsWithHandshake(absent, []map[string]receiveRouteSelection{{}}, nil, own, false, map[string]string{}); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	if err := commitWithdrawnSiteCaches(forwarding, advertisements); err != nil {
		return err
	}
	if follower.Version != 0 {
		next := follower
		next.Routes = maps.Clone(follower.Routes)
		delete(next.Routes, target.RootPublicKey)
		return replaceReceiveFollowerCache(followerPath, cfg, followerScope, own, follower, next)
	}
	return nil
}
