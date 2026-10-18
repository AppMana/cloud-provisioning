package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Caller holds the host lock. Retire the journal's site receivers and separately
// prepared successors recorded in the admitted ownership directory. Ordinary
// root peers, source exceptions, relay paths and other caches remain separate
// cleanup obligations; success here must never be published as full retirement.
func retireAdmittedSiteReceiversLocked(ctx context.Context, api kubernetes.Interface, devices receiverDevices, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, lockPath string, admitted admittedReceiveRetirement) error {
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
	own, _, err := tunnel.ReceiveLocalNetwork(plan, admitted.NodeUID)
	if err != nil {
		return err
	}
	if err := checkpointReceiveRetirementLocked(ctx, api, store, scope, cfg, key, lockPath, admitted); err != nil {
		return err
	}
	path := siteReceiveCachePath(cfg) + ".admitted"
	graphs := []tunnel.ReceiveTransitionPlan{plan}
	if plan.PreviousReceiver != nil {
		graphs = append(graphs, *plan.PreviousReceiver.Plan)
	}
	// Preserve committed receivers alongside additive preparation records before
	// changing routes. An interrupted cleanup must retain both sources of intent.
	for _, graph := range graphs {
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
			routes := map[string]receiveRouteSelection{}
			for _, prefix := range destinations[name] {
				routes[prefix] = selection
			}
			generations = append(generations, routes)
		}
		return withdrawReceiveDeviceRoutes(ctx, cfg, generations, own)
	}
	return retireSiteReceiverRecords(ctx, api, cfg, mesh, key, path, guard, plan.RootPublicKey, prepare, func(selections []receiveRouteSelection, _ []string) error {
		return retireRecordedSiteReceiver(devices, cfg, selections)
	})
}
