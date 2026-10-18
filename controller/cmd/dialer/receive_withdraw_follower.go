package main

import (
	"context"
	"fmt"
	"os"
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
	check := func() error {
		current, err := loadReceiveRetirement(ctx, api, store, scope, cfg.nodeName, key)
		if err != nil {
			return err
		}
		if !sameReceiveRetirement(admitted, current) {
			return fmt.Errorf("follower retirement authority changed")
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
	if _, err := tunnel.PlanReceiveWithdrawnNeighborPeers(withdrawnTransitPeerVariants(forwarding, admitted.Plans, target.RootPublicKey), currentPeers, admitted.Plans, target.RootPublicKey); err != nil {
		return err
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
	if speaker != nil {
		prefixes := map[string]bool{}
		for raw := range routes {
			prefixes[raw] = true
		}
		if err := speaker.withdrawReceivePrefixes(ctx, prefixes); err != nil {
			return err
		}
	}
	if err := withdrawReceiveDeviceRoutes(ctx, cfg, []map[string]receiveRouteSelection{{}}, own, routes); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	return commitWithdrawnSiteCaches(forwarding, advertisements)
}
