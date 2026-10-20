package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"reflect"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

type receiveFollowerNative func(context.Context, wgtypes.Key, string, receiveObserverAdmission, func() error) error

// The observer executor holds the host lock and admits the complete live history.
// Only committed worker paths enter this writer; missing workers remain recorded
// for explicit retirement rather than being inferred absent from a partial plan.
func reconcileReceiveFollower(ctx context.Context, api kubernetes.Interface, cfg config, speaker *transitSpeaker, key wgtypes.Key, lockPath string, admitted receiveObserverAdmission, check func() error) error {
	var own []string
	for _, address := range admitted.Node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP && address.Address != "" {
			own = append(own, address.Address)
		}
	}
	sources := tunnel.SplitList(string(admitted.Mesh.Data[tunnel.NodeAddressesPrefix+cfg.nodeName]), string(admitted.Mesh.Data[tunnel.SiteAddressesPrefix+cfg.nodeName]))
	if len(own) == 0 || !sameAdvertisementSet(own, sources) {
		return fmt.Errorf("follower mesh sources differ from current Node addresses")
	}
	desired := map[string]map[string]string{}
	plans := []tunnel.ReceiveTransitionPlan{}
	// Rebuild this check at every mutation boundary. A receipt or round can expire
	// without changing an API resource version during otherwise successful work.
	checkPaths := func() error {
		for _, uid := range admitted.Inventory.MachineUIDs {
			if len(admitted.Inventory.Publications[uid].Retirement) != 0 {
				continue
			}
			path, err := admitReceiveFollowerJournalPath(admitted.Mesh.Data, cfg.nodeName, string(admitted.Node.UID), admitted.Journals[uid], admitted.Inventory.ClusterUID, time.Now(), func(owner tunnel.ReceiveTransitionSite, receipt handover.Receipt) error {
				return handover.VerifyObservationIncarnation(ctx, api, admitted.Mesh.Namespace, owner.NodeName, receipt)
			})
			if err != nil {
				return err
			}
			plan, err := tunnel.DecodeReceiveTransition(admitted.Journals[uid], admitted.Inventory.ClusterUID)
			if err != nil {
				return err
			}
			routes, err := receiveFollowerTransitRoutes(path.Transit)
			if err != nil {
				return err
			}
			if old, found := desired[plan.RootPublicKey]; found && !reflect.DeepEqual(old, routes) {
				return fmt.Errorf("follower selection changed during execution")
			}
			desired[plan.RootPublicKey] = routes
		}
		return ctx.Err()
	}
	for _, uid := range admitted.Inventory.MachineUIDs {
		plan, err := tunnel.DecodeReceiveTransition(admitted.Journals[uid], admitted.Inventory.ClusterUID)
		if err != nil {
			return err
		}
		plans = append(plans, plan)
	}
	if err := checkPaths(); err != nil {
		return err
	}
	if len(desired) == 0 {
		return check()
	}
	peers, err := tunnel.SitePeers(admitted.Mesh.Data)
	if err != nil {
		return err
	}
	boot, err := localReceiveBootID()
	if err != nil {
		return err
	}
	expected := receiveInventoryCheckpoint{Version: 1, InventoryUID: string(admitted.Metadata.UID), Inventory: admitted.Inventory, NodeName: cfg.nodeName, NodeUID: string(admitted.Node.UID), BootID: boot, PublicKey: key.PublicKey().String()}
	return applyReceiveFollowerRoutes(ctx, cfg, speaker, key, lockPath, expected, own, desired, peers, plans, func() error {
		if err := check(); err != nil {
			return err
		}
		return checkPaths()
	})
}

func receiveFollowerTransitRoutes(transit *tunnel.TransitSpec) (map[string]string, error) {
	if transit == nil {
		return nil, fmt.Errorf("follower requires committed relay destinations")
	}
	if err := validateSiteTransit(transit); err != nil {
		return nil, err
	}
	routes := map[string]string{}
	prefixes := slices.Clone(transit.Blocks)
	for _, host := range transit.Hosts {
		prefixes = append(prefixes, tunnel.HostCIDR(host))
	}
	for _, raw := range prefixes {
		_, dst, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, err
		}
		via := transit.Gateway(dst.IP)
		if via == nil {
			return nil, fmt.Errorf("follower relay lacks destination address family")
		}
		routes[raw] = via.String()
	}
	return routes, nil
}

// Native ownership is admitted from private durable records, never guessed from
// the route table. Stage possible writes before installation, retain them on any
// error, and compact only after scoped readback and renewed live authorization.
func applyReceiveFollowerRoutes(ctx context.Context, cfg config, speaker *transitSpeaker, key wgtypes.Key, lockPath string, expected receiveInventoryCheckpoint, own []string, desired map[string]map[string]string, peers []tunnel.PeerSpec, plans []tunnel.ReceiveTransitionPlan, check func() error) error {
	if check == nil || len(desired) == 0 {
		return fmt.Errorf("follower installation requires live authority and destinations")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := netlink.LinkByName(cfg.iface); !isLinkNotFound(err) {
		return fmt.Errorf("follower retains local tunnel state: %v", err)
	}
	base := siteReceiveCachePath(cfg)
	for _, path := range []string{base, base + ".admitted", base + ".owners", base + ".admitted.owners", lockPath + ".neighbors.json", lockPath + ".neighbors.json.retiring"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return fmt.Errorf("follower retains unresolved receiver state")
		}
	}
	raw, err := readPrivateRetirementCache(siteForwardingCachePath(cfg))
	if err != nil {
		return err
	}
	saved, err := decodeSiteForwardingCache(raw, cfg, key)
	if err != nil {
		return err
	}
	if !saved.Transit || saved.MeshUID != expected.Inventory.MeshUID || !slices.Equal(saved.Own, own) {
		return fmt.Errorf("follower forwarding ownership differs")
	}
	path := lockPath + ".follower.json"
	previous, err := readReceiveFollowerCache(path, cfg, expected, own)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// Read the old whole-mesh election only as evidence of routes installed before
	// per-worker selection. It cannot authorize the desired generation.
	oldTransit, err := tunnel.SiteTransit(saved.Data, saved.NotReady)
	if err != nil {
		return err
	}
	oldRoutes, err := receiveFollowerTransitRoutes(oldTransit)
	if err != nil {
		return err
	}
	oldPeers, err := tunnel.SitePeers(saved.Data)
	if err != nil {
		return err
	}
	evidence := map[string][]map[string]string{}
	staging := map[string][]map[string]string{}
	// Import all legacy owners, including workers this pass will not touch.
	// Otherwise an omitted worker's old allocation could be claimed by a new
	// worker before the explicit retirement protocol removes its evidence.
	for _, peer := range oldPeers {
		if !peer.Remote {
			continue
		}
		legacy := map[string]string{}
		prefixes := slices.Clone(peer.WGAllowedIPs)
		for _, host := range peer.AllRouteHosts() {
			prefixes = append(prefixes, tunnel.HostCIDR(host))
		}
		for _, prefix := range prefixes {
			if via, found := oldRoutes[prefix]; found {
				legacy[prefix] = via
			}
		}
		if len(legacy) != 0 {
			staging[peer.PublicKey] = append(staging[peer.PublicKey], legacy)
		}
	}
	for root := range desired {
		evidence[root] = append(evidence[root], previous.Routes[root]...)
		evidence[root] = append(evidence[root], staging[root]...)
		evidence[root] = append(evidence[root], desired[root])
		staging[root] = evidence[root]
	}
	cfg.receiveRoutes = nil
	cfg.receiveRelayRoutes = map[string]string{}
	cfg.receiveRouteScope = map[string]bool{}
	var variants []map[string]string
	var hosts []net.IPNet
	for root, generations := range evidence {
		// Reuse retirement's ownership overlap check, excluding this root itself.
		// This grants neither retirement authority nor permission to change a peer.
		other := []tunnel.PeerSpec{}
		for _, peer := range peers {
			if peer.PublicKey != root {
				other = append(other, peer)
			}
		}
		for _, routes := range generations {
			recorded := tunnel.PeerSpec{PublicKey: root, Remote: true}
			for prefix := range routes {
				recorded.WGAllowedIPs = append(recorded.WGAllowedIPs, prefix)
				cfg.receiveRouteScope[prefix] = true
			}
			if _, err := tunnel.PlanReceiveWithdrawnNeighborPeers([]tunnel.PeerSpec{recorded}, other, plans, root); err != nil {
				return err
			}
			variants = append(variants, routes)
		}
		for prefix, gateway := range desired[root] {
			cfg.receiveRelayRoutes[prefix] = gateway
			_, dst, err := net.ParseCIDR(prefix)
			if err != nil {
				return err
			}
			hosts = append(hosts, *dst)
		}
	}
	var advertisements transitAdvertisementCache
	if speaker != nil {
		advertisements, _, err = readTransitAdvertisements(cfg, speaker)
		if err != nil {
			return err
		}
	}
	expectedRecord := previous
	renew := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := readPrivateRetirementCache(siteForwardingCachePath(cfg))
		if err != nil {
			return err
		}
		if !bytes.Equal(current, raw) {
			return fmt.Errorf("follower legacy ownership changed")
		}
		record, err := readReceiveFollowerCache(path, cfg, expected, own)
		if expectedRecord.Version == 0 {
			if !os.IsNotExist(err) {
				return fmt.Errorf("follower ownership appeared during admission")
			}
		} else if err != nil || !reflect.DeepEqual(record, expectedRecord) {
			return fmt.Errorf("follower ownership changed during execution: %v", err)
		}
		if speaker != nil {
			currentAds, _, err := readTransitAdvertisements(cfg, speaker)
			if err != nil || !reflect.DeepEqual(currentAds, advertisements) {
				return fmt.Errorf("follower advertisement ownership changed: %v", err)
			}
		}
		return check()
	}
	if err := renew(); err != nil {
		return err
	}
	if err := preflightReceiveRouteGenerationsWithHandshake(cfg, []map[string]receiveRouteSelection{{}}, nil, own, false, variants...); err != nil {
		return err
	}
	// Persist legacy candidates too: retirement must understand an interrupted
	// migration even when the whole-mesh cache has subsequently been compacted.
	staged, err := stageReceiveFollowerRouteVariants(path, cfg, expected, own, previous, staging)
	if err != nil {
		return err
	}
	expectedRecord = staged
	if err := renew(); err != nil {
		return err
	}
	if err := preflightReceiveRouteGenerationsWithHandshake(cfg, []map[string]receiveRouteSelection{{}}, nil, own, false, variants...); err != nil {
		return err
	}
	if speaker != nil {
		removed := map[string]bool{}
		for prefix := range cfg.receiveRouteScope {
			if _, remains := cfg.receiveRelayRoutes[prefix]; !remains {
				removed[prefix] = true
			}
		}
		if err := speaker.withdrawReceivePrefixes(ctx, removed); err != nil {
			return err
		}
	}
	if err := installRoutes(cfg, hosts, nil, nil, nil, nil, own); err != nil {
		return err
	}
	if err := verifyReceiveRouteInstallation(cfg, hosts, nil, own); err != nil {
		return err
	}
	if err := renew(); err != nil {
		return err
	}
	if speaker != nil {
		routes := []transitRoute{}
		for prefix, med := range speaker.advertised {
			if !cfg.receiveRouteScope[prefix] {
				routes = append(routes, transitRoute{prefix: prefix, med: med})
			}
		}
		for prefix := range cfg.receiveRelayRoutes {
			routes = append(routes, transitRoute{prefix: prefix, med: endpointRank(saved.Data, cfg.nodeName)})
		}
		if err := speaker.reconcile(ctx, advertisements.Peers, routes); err != nil {
			return err
		}
	}
	if err := renew(); err != nil {
		return err
	}
	_, err = finishReceiveFollowerRoutes(path, cfg, expected, own, staged, desired)
	return err
}
