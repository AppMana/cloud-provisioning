package main

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The observer has completed serving and its final live check, still under the
// host lock. A mixed transition may serve correctly without matching the global
// legacy projection; in that case withhold the whole-site acknowledgement.
func acknowledgeReceiveFollower(ctx context.Context, api kubernetes.Interface, cfg config, key wgtypes.Key, lockPath string, admitted receiveObserverAdmission, check func() error) error {
	peers, err := tunnel.SitePeers(admitted.Mesh.Data)
	if err != nil {
		return err
	}
	plans := []tunnel.ReceiveTransitionPlan{}
	for _, uid := range admitted.Inventory.MachineUIDs {
		if len(admitted.Inventory.Publications[uid].Retirement) != 0 {
			return nil
		}
		plan, err := tunnel.DecodeReceiveTransition(admitted.Journals[uid], admitted.Inventory.ClusterUID)
		if err != nil {
			return err
		}
		plans = append(plans, plan)
	}
	desired, pending, checkOrdinary, err := admitReceiveFollowerOrdinary(ctx, api, cfg, admitted, peers, plans)
	if err != nil {
		return err
	}
	if pending {
		return nil
	}
	for _, plan := range plans {
		path, err := admitReceiveFollowerJournalPath(admitted.Mesh.Data, cfg.nodeName, string(admitted.Node.UID), admitted.Journals[plan.MachineUID], admitted.Inventory.ClusterUID, time.Now(), func(owner tunnel.ReceiveTransitionSite, receipt handover.Receipt) error {
			return handover.VerifyObservationIncarnation(ctx, api, admitted.Mesh.Namespace, owner.NodeName, receipt)
		})
		if err != nil {
			return err
		}
		desired[plan.RootPublicKey], err = receiveFollowerTransitRoutes(path.Transit)
		if err != nil {
			return err
		}
	}
	own := []string{}
	for _, address := range admitted.Node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP && address.Address != "" {
			own = append(own, address.Address)
		}
	}
	if !sameAdvertisementSet(own, tunnel.SplitList(string(admitted.Mesh.Data[tunnel.NodeAddressesPrefix+cfg.nodeName]), string(admitted.Mesh.Data[tunnel.SiteAddressesPrefix+cfg.nodeName]))) {
		return fmt.Errorf("follower acknowledgement source changed")
	}
	boot, err := localReceiveBootID()
	if err != nil {
		return err
	}
	expected := receiveInventoryCheckpoint{Version: 1, InventoryUID: string(admitted.Metadata.UID), Inventory: admitted.Inventory, NodeName: cfg.nodeName, NodeUID: string(admitted.Node.UID), BootID: boot, PublicKey: key.PublicKey().String()}
	saved, err := readReceiveFollowerCache(lockPath+".follower.json", cfg, expected, own)
	if err != nil {
		return err
	}
	if len(saved.Routes) != len(peers) || len(desired) != len(peers) {
		return nil
	}
	selected := map[string]string{}
	for _, peer := range peers {
		variants := saved.Routes[peer.PublicKey]
		if saved.Retired[peer.PublicKey] {
			return fmt.Errorf("retired follower key cannot acknowledge forwarding")
		}
		if len(variants) != 1 || !reflect.DeepEqual(variants[0], desired[peer.PublicKey]) {
			return nil
		}
		for raw, via := range variants[0] {
			selected[raw] = via
		}
	}
	nodes, err := api.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	notReady := map[string]bool{}
	for _, node := range nodes.Items {
		ready := false
		for _, condition := range node.Status.Conditions {
			ready = ready || (condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue)
		}
		notReady[node.Name] = !ready
	}
	role, transit, err := tunnel.SiteForwardingRole(admitted.Mesh.Data, cfg.nodeName, notReady)
	if err != nil {
		return err
	}
	if role != "transit" {
		return fmt.Errorf("follower acknowledgement role changed")
	}
	global, err := receiveFollowerTransitRoutes(transit)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(selected, global) {
		return nil
	}
	cfg.receiveRoutes = nil
	cfg.receiveRelayRoutes = selected
	cfg.receiveRouteScope = map[string]bool{}
	hosts := []net.IPNet{}
	for raw := range selected {
		cfg.receiveRouteScope[raw] = true
		_, dst, err := net.ParseCIDR(raw)
		if err != nil {
			return err
		}
		hosts = append(hosts, *dst)
	}
	// Whole-site deletion cannot be acknowledged with any unaccounted route,
	// including one reappearing after an ordinary key's record was compacted.
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.Dst == nil || !cfg.receiveRouteScope[route.Dst.String()] {
			return fmt.Errorf("unaccounted route prevents whole-site acknowledgement")
		}
	}
	if len(hosts) != 0 {
		if err := verifyReceiveRouteInstallation(cfg, hosts, nil, own); err != nil {
			return err
		}
	}
	if err := check(); err != nil {
		return err
	}
	if err := checkOrdinary(); err != nil {
		return err
	}
	hash, err := tunnel.SitePeerHash(admitted.Mesh.Data)
	if err != nil {
		return err
	}
	return acknowledgeSite(ctx, api, admitted.Mesh.Namespace, admitted.Mesh.Name, cfg.nodeName, string(admitted.Node.UID), "", string(admitted.Mesh.UID), hash, transit)
}
