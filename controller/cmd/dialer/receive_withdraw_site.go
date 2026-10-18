package main

import (
	"context"
	"fmt"
	"net"
	"sort"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Caller holds the host lock. Retire the journal's site receivers and separately
// prepared successors recorded in both receiver ownership directories. Ordinary
// source exceptions, committed relay routes, root peers and caches are also
// retired. Non-endpoint followers and authenticated completion remain separate obligations; success here must
// never be published as full retirement.
func retireAdmittedSiteReceiversLocked(ctx context.Context, api kubernetes.Interface, devices receiverDevices, speaker *transitSpeaker, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, lockPath string, admitted admittedReceiveRetirement) error {
	if devices == nil || cfg.secretName != scope.MeshName || cfg.secretNamespace != scope.Namespace || siteReceiveCachePath(cfg) == "" {
		return fmt.Errorf("site retirement requires native devices and local mesh storage")
	}
	current, err := loadReceiveRetirement(ctx, api, store, scope, cfg.nodeName, key)
	if err != nil {
		return err
	}
	if !sameReceiveRetirement(admitted, current) {
		return fmt.Errorf("site retirement authority changed")
	}
	admitted = current
	plan, err := tunnel.DecodeReceiveTransition(admitted.Journals[scope.MachineUID], store.ClusterUID)
	if err != nil {
		return err
	}
	member, found := plan.Sites[key.PublicKey().String()]
	if !found || member.NodeUID != admitted.NodeUID || member.NodeName != cfg.nodeName {
		return fmt.Errorf("site retirement requires the retained receiver participant")
	}
	own, allocated, err := tunnel.ReceiveLocalNetwork(plan, admitted.NodeUID)
	if err != nil {
		return err
	}
	if err := checkpointReceiveRetirementLocked(ctx, api, store, scope, cfg, key, lockPath, admitted); err != nil {
		return err
	}
	caches, err := preserveRetiringSiteCaches(cfg, key, scope.MeshUID)
	if err != nil {
		return err
	}
	forwarding, err := prepareWithdrawnForwardingCache(cfg, key, scope.MeshUID, plan.MachineName, plan.RootPublicKey)
	if err != nil {
		return err
	}
	var recorded []tunnel.PeerSpec
	if forwarding != nil {
		recorded = append(recorded, forwarding.peers...)
	}
	for _, cache := range caches {
		recorded = append(recorded, cache.record.Peers.Peers...)
		for _, previous := range cache.record.Previous {
			recorded = append(recorded, previous.Peers...)
		}
	}
	basePath := siteReceiveCachePath(cfg)
	path := basePath + ".admitted"
	graphs := []tunnel.ReceiveTransitionPlan{plan}
	if plan.PreviousReceiver != nil {
		graphs = append(graphs, *plan.PreviousReceiver.Plan)
	}
	// Preserve committed receivers alongside additive preparation records before
	// changing routes. An interrupted cleanup must retain both sources of intent.
	for _, graph := range graphs {
		recorded = append(recorded, graph.SiteDesired.Peers...)
		recorded = append(recorded, graph.PreviousSitePeers().Peers...)
		root := graph.RootPublicKey
		record := siteReceiveCache{Version: 1, Namespace: scope.Namespace, SecretName: scope.MeshName, MeshUID: scope.MeshUID, NodeName: cfg.nodeName, PublicKey: key.PublicKey().String(), Peers: graph.SiteDesired, Bindings: map[string]receiveBinding{root: graph.Bindings[key.PublicKey().String()]}, Previous: map[string]tunnel.PeerListDoc{root: graph.PreviousSitePeers()}}
		if graph.ReceiverGeneration != "" {
			record.Generations = map[string]string{root: graph.ReceiverGeneration}
		}
		if err := preserveSiteReceiveOwnership(path, record, cfg, key); err != nil {
			return err
		}
	}
	mesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !sameReceiveRevision(mesh.ObjectMeta, admitted.MeshMetadata) {
		return fmt.Errorf("withdrawal mesh changed before site cleanup")
	}
	check := func() error {
		current, err := loadReceiveRetirement(ctx, api, store, scope, cfg.nodeName, key)
		if err != nil {
			return err
		}
		if !sameReceiveRetirement(admitted, current) {
			return fmt.Errorf("site retirement authority changed during cleanup")
		}
		return ctx.Err()
	}
	currentPeers, err := tunnel.SitePeers(mesh.Data)
	if err != nil {
		return err
	}
	if currentPeers == nil {
		currentPeers = []tunnel.PeerSpec{}
	}
	if _, err := tunnel.PlanReceiveWithdrawnNeighborPeers(recorded, currentPeers, admitted.Plans, plan.RootPublicKey); err != nil {
		return err
	}
	sourceCfg, sourceAddresses, sourceHosts, err := planReceiveSourceWithdrawal(cfg, admitted.Plans, plan.RootPublicKey, admitted.NodeUID)
	if err != nil {
		return err
	}
	var relayGenerations []map[string]string
	for _, graph := range graphs {
		routes, err := tunnel.PlanReceiveRoutes(graph)
		if err != nil {
			return err
		}
		member := graph.Sites[key.PublicKey().String()]
		for _, path := range []*tunnel.ReceiveSitePath{member.Legacy, member.Forward} {
			if path == nil || path.Transit == nil {
				continue
			}
			relays := map[string]string{}
			for raw := range routes[admitted.NodeUID] {
				_, dst, err := net.ParseCIDR(raw)
				if err != nil {
					return err
				}
				gateway := path.Transit.Gateway(dst.IP)
				if gateway == nil {
					return fmt.Errorf("withdrawn relay has no committed gateway")
				}
				relays[raw] = gateway.String()
			}
			relayGenerations = append(relayGenerations, relays)
		}
	}
	guard := &siteReceiveScope{plans: admitted.Plans, check: check}
	prepare := func(groups map[string][]receiveRouteSelection, destinations map[string][]string) error {
		var names []string
		for name := range groups {
			names = append(names, name)
		}
		sort.Strings(names)
		var generations []map[string]receiveRouteSelection
		for _, name := range names {
			selection, err := selectRecordedSiteReceiver(devices, cfg, groups[name])
			if err != nil {
				return err
			}
			// Add recorded pending generations for committed source hosts only.
			// Exact installed permissions were checked by selectRecordedSiteReceiver.
			for _, host := range sourceHosts {
				accepts := false
				for _, raw := range selection.AllowedSources {
					_, prefix, e := net.ParseCIDR(raw)
					if e != nil {
						return e
					}
					accepts = accepts || prefix.Contains(host.IP)
				}
				if !accepts {
					continue
				}
				candidates := sourceCfg.receiveSourceCandidates[host.String()]
				replaced := false
				for i := range candidates {
					if candidates[i].Interface == selection.Interface {
						candidates[i] = selection
						replaced = true
					}
				}
				if !replaced {
					candidates = append(candidates, selection)
				}
				sourceCfg.receiveSourceCandidates[host.String()] = candidates
			}
			routes := map[string]receiveRouteSelection{}
			for _, prefix := range destinations[name] {
				routes[prefix] = selection
			}
			generations = append(generations, routes)
		}
		if err := withdrawReceiveSourceRoutes(ctx, sourceCfg, sourceAddresses, sourceHosts); err != nil {
			return err
		}
		if err := check(); err != nil {
			return err
		}
		return withdrawReceiveDeviceRoutes(ctx, cfg, generations, own, relayGenerations...)
	}
	// Stop announcing target destinations before removing their forwarding.
	// The retirement checkpoint fences offline cache replay across interruption.
	if err := check(); err != nil {
		return err
	}
	if err := withdrawReceiveAdvertisements(ctx, speaker, admitted.Plans, scope.MachineUID, admitted.NodeUID); err != nil {
		return err
	}
	checkpoint := receiveNeighborCheckpoint{Version: 1, InventoryUID: string(admitted.Metadata.UID), MeshUID: scope.MeshUID, MeshVersion: mesh.ResourceVersion, NodeUID: admitted.NodeUID, BootID: admitted.BootID, PublicKey: key.PublicKey().String(), Peers: currentPeers}
	if err := retireWithdrawnNeighborCaches(ctx, lockPath+".neighbors.json", checkpoint, admitted.Plans, plan.RootPublicKey, recorded, check, func(peers []tunnel.PeerSpec) error {
		// The sidecar has durably imported cache-only permission variants before
		// receiver cleanup can remove their original executable references.
		if err := retireSiteReceiverRecords(ctx, api, cfg, mesh, key, path, guard, plan.RootPublicKey, prepare, func(selections []receiveRouteSelection, _ []string) error {
			return retireRecordedSiteReceiver(devices, cfg, selections)
		}, basePath); err != nil {
			return err
		}
		if err := commitRetiredSiteCaches(caches, plan.RootPublicKey); err != nil {
			return err
		}
		return retireReceiveNeighbors(ctx, devices, cfg, key, peers, own, allocated)
	}); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	return commitWithdrawnForwardingCache(forwarding)
}
