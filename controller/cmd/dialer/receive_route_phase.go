package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// receiveRoutePhase implements the direct-routing switch and drain operations.
// Admission, durable checkpointing and the host lock belong to executeReceivePhase.
// It does not prepare or retire transports; unsupported phases cannot produce a
// receipt. own must be the caller-admitted stable source-address allocation.
func receiveRoutePhase(ctx context.Context, devices receiverDevices, cfg config, key wgtypes.Key, own []string, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if view.Complete || (view.Round.Phase != handover.Switch && view.Round.Phase != handover.Drain) {
		return fmt.Errorf("native receiver route backend does not implement this phase")
	}
	if devices == nil || key == (wgtypes.Key{}) || nodeUID == "" {
		return fmt.Errorf("native receiver route phase requires local identity")
	}
	expected, err := tunnel.ReceiveTransitionSpec(plan, view.Round.ClusterUID, view.Round.PlanHash)
	if err != nil {
		return err
	}
	expected.Phase = view.Round.Phase
	if view.RollbackRequested {
		expected.FromGeneration, expected.ToGeneration = expected.ToGeneration, expected.FromGeneration
		for i := range expected.Members {
			m := &expected.Members[i]
			m.FromKey, m.ToKey = m.ToKey, m.FromKey
			m.FromPeerKey, m.ToPeerKey = m.ToPeerKey, m.FromPeerKey
		}
	}
	if !reflect.DeepEqual(expected, view.Round.Spec) || !reflect.DeepEqual(intent.Plans[plan.MachineUID], plan) {
		return fmt.Errorf("native phase differs from admitted transport plan")
	}
	scope := receiveJournalScope{Namespace: plan.Namespace, MeshName: plan.MeshName, MeshUID: plan.MeshUID, MachineUID: plan.MachineUID}
	uid, err := receiveJournalParticipant(plan, scope, cfg.nodeName, key)
	if err != nil || uid != nodeUID {
		return fmt.Errorf("native phase participant differs: %v", err)
	}
	cfg.receiveRoutes = intent.Receivers[nodeUID]
	cfg.receiveRouteScope = map[string]bool{}
	desired := map[string]bool{}
	for raw := range cfg.receiveRoutes {
		desired[raw] = true
	}
	legacyPeers := map[string]tunnel.PeerSpec{}
	var all []tunnel.ReceiveTransitionPlan
	var receiverPlans []tunnel.ReceiveTransitionPlan
	for machineUID, p := range intent.Plans {
		if p.MachineUID != machineUID {
			return fmt.Errorf("native phase inventory identity mismatch")
		}
		all = append(all, p)
		routes, err := tunnel.PlanReceiveRoutes(p)
		if err != nil {
			return err
		}
		for raw := range routes[nodeUID] {
			cfg.receiveRouteScope[raw] = true
		}
		var old []tunnel.PeerSpec
		if nodeUID == p.CloudNodeUID {
			var base tunnel.PeerListDoc
			if err := json.Unmarshal(p.Base, &base); err != nil {
				return err
			}
			for _, peer := range base.Peers {
				if !peer.Remote {
					old = append(old, peer)
				}
			}
		} else {
			for _, site := range p.Sites {
				if site.NodeUID == nodeUID {
					for _, peer := range p.SiteDesired.Peers {
						if peer.PublicKey == p.RootPublicKey {
							old = append(old, peer)
						}
					}
				}
			}
		}
		selected, legacy := intent.Legacy[machineUID]
		if legacy && !reflect.DeepEqual(selected, p) {
			return fmt.Errorf("legacy selection differs from admitted plan")
		}
		if !legacy {
			receiverPlans = append(receiverPlans, p)
		}
		for _, peer := range old {
			destinations := append([]string(nil), peer.WGAllowedIPs...)
			for _, host := range peer.AllRouteHosts() {
				destinations = append(destinations, tunnel.HostCIDR(host))
			}
			for _, raw := range destinations {
				cfg.receiveRouteScope[raw] = true
				if legacy {
					desired[raw] = true
				}
			}
			if legacy || machineUID == plan.MachineUID {
				legacyPeers[peer.PublicKey] = peer
			}
		}
	}
	allRoutes, err := tunnel.PlanReceiveRouteSet(all)
	if err != nil {
		return err
	}
	for uid := range intent.Legacy {
		if _, found := intent.Plans[uid]; !found {
			return fmt.Errorf("legacy selection has no admitted plan")
		}
	}
	expectedRoutes := map[string]tunnel.ReceiveRouteSelection{}
	if len(receiverPlans) != 0 {
		routes, err := tunnel.PlanReceiveRouteSet(receiverPlans)
		if err != nil {
			return err
		}
		if routes[nodeUID] != nil {
			expectedRoutes = routes[nodeUID]
		}
	}
	if len(expectedRoutes) != len(cfg.receiveRoutes) || (len(expectedRoutes) != 0 && !reflect.DeepEqual(expectedRoutes, cfg.receiveRoutes)) {
		return fmt.Errorf("native receiver selections differ from admitted plans")
	}
	_, targetLegacy := intent.Legacy[plan.MachineUID]
	if targetLegacy != view.RollbackRequested {
		return fmt.Errorf("native route direction differs from committed phase")
	}
	live, err := devices.Device(cfg.iface)
	if err != nil {
		return err
	}
	if live.PublicKey != key.PublicKey() || live.FirewallMark != cfg.fwmark {
		return fmt.Errorf("legacy receive generation identity changed")
	}
	if len(legacyPeers) != 0 {
		for public, expected := range legacyPeers {
			found := false
			for _, peer := range live.Peers {
				if peer.PublicKey.String() == public {
					found = receiveSourcesMatch(expected.WGAllowedIPs, peer.AllowedIPs) && peer.Endpoint != nil && !peer.LastHandshakeTime.IsZero() && !peer.LastHandshakeTime.After(time.Now()) && time.Since(peer.LastHandshakeTime) <= receiveHandshakeMaxAge
				}
			}
			if !found {
				return fmt.Errorf("selected legacy peer or source permissions changed")
			}
		}
	}
	var hosts []net.IPNet
	for raw := range desired {
		_, dst, err := net.ParseCIDR(raw)
		if err != nil {
			return err
		}
		hosts = append(hosts, *dst)
	}
	if err := validateReceiveRouteScope(cfg, hosts, nil, nil, nil, nil); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyReceivePhasePaths(devices, cfg, key, plan, nodeUID); err != nil {
		return err
	}
	if view.Round.Phase == handover.Switch {
		if err := preflightReceiveRoutePhase(cfg, allRoutes[nodeUID], own); err != nil {
			return err
		}
		if err := installRoutes(cfg, hosts, nil, nil, nil, nil, own); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return verifyReceiveRouteInstallation(cfg, hosts, nil, own)
}

// A cloud participant can have retained receive paths with no desired egress.
// Verify those members too before the execution boundary can issue their receipts.
func verifyReceivePhasePaths(devices receiverDevices, cfg config, key wgtypes.Key, plan tunnel.ReceiveTransitionPlan, nodeUID string) error {
	var selections []receiveRouteSelection
	if nodeUID == plan.CloudNodeUID {
		var base tunnel.PeerListDoc
		if err := json.Unmarshal(plan.Base, &base); err != nil {
			return err
		}
		paths, err := tunnel.PlanReceiveHandover(plan.MeshUID, base, plan.Desired, plan.Native)
		if err != nil {
			return err
		}
		for _, path := range paths.Receivers {
			binding := plan.Bindings[path.PublicKey]
			if binding.PublicKey != receiverKey(key, plan.MeshUID, path.PublicKey).PublicKey().String() {
				return fmt.Errorf("receiver phase key differs from local derivation")
			}
			live, err := devices.Device(binding.Interface)
			if err != nil {
				return err
			}
			if live.ListenPort != binding.ListenPort {
				return fmt.Errorf("receiver phase listener differs from reserved port")
			}
			selections = append(selections, receiveRouteSelection{Interface: binding.Interface, Alias: receiverAlias(plan.MeshUID, path.PublicKey), PublicKey: binding.PublicKey, PeerPublicKey: path.PublicKey, AllowedSources: path.AllowedSources})
		}
	} else {
		site, found := plan.Sites[key.PublicKey().String()]
		if !found || site.NodeUID != nodeUID {
			return fmt.Errorf("native site receive identity is missing")
		}
		handshake, err := siteReceiveHandshake(devices, key, site.Client, cfg.fwmark, time.Now())
		if err != nil || handshake.IsZero() {
			return fmt.Errorf("native site receive transport is not ready: %v", err)
		}
		routes, err := tunnel.PlanReceiveRoutes(plan)
		if err != nil {
			return err
		}
		for _, selection := range routes[nodeUID] {
			selections = append(selections, selection)
			break // A site has one receive client for this cloud journal.
		}
	}
	if len(selections) == 0 {
		return fmt.Errorf("native phase has no local receive paths")
	}
	for _, selection := range selections {
		if len(selection.AllowedSources) == 0 {
			return fmt.Errorf("native receive path has no source permissions")
		}
		raw := selection.AllowedSources[0]
		_, prefix, err := net.ParseCIDR(raw)
		if err != nil {
			return err
		}
		check := cfg
		check.receiveRoutes = map[string]receiveRouteSelection{raw: selection}
		if _, err := validateReceiveRouteSelections(check, []net.IPNet{*prefix}, nil); err != nil {
			return err
		}
	}
	return nil
}

// Only the admitted legacy device or the exact prepared receiver may already
// own a scoped destination. Reject foreign routes and more-specific shadows
// before installRoutes can replace anything. Unrelated destinations survive.
func preflightReceiveRoutePhase(cfg config, receivers map[string]receiveRouteSelection, own []string) error {
	legacy, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return err
	}
	if legacy.Type() != "wireguard" {
		return fmt.Errorf("legacy route device is not WireGuard")
	}
	local := localIPs()
	seen := map[string]bool{}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		for _, route := range routes {
			if route.Dst == nil {
				continue
			}
			raw := route.Dst.String()
			if !cfg.receiveRouteScope[raw] {
				prefix, err := netip.ParsePrefix(raw)
				if err != nil {
					return err
				}
				for scoped := range cfg.receiveRouteScope {
					parent, _ := netip.ParsePrefix(scoped)
					if prefix.Addr().BitLen() == parent.Addr().BitLen() && prefix.Bits() > parent.Bits() && parent.Contains(prefix.Addr()) {
						return fmt.Errorf("foreign more-specific route shadows receiver scope")
					}
				}
				continue
			}
			if seen[raw] || route.Type != unix.RTN_UNICAST || route.Protocol != unix.RTPROT_BOOT || route.Priority != 0 || len(route.Gw) != 0 || len(route.MultiPath) != 0 || route.Via != nil || route.Encap != nil || route.NewDst != nil || route.Tos != 0 || !route.Src.Equal(transitSource(route.Dst.IP, own, local)) {
				return fmt.Errorf("scoped destination has conflicting route ownership")
			}
			seen[raw] = true
			if route.LinkIndex == legacy.Attrs().Index {
				continue
			}
			selection, found := receivers[raw]
			if !found {
				return fmt.Errorf("scoped route uses an unadmitted device")
			}
			check := cfg
			check.receiveRoutes = map[string]receiveRouteSelection{raw: selection}
			indices, err := validateReceiveRouteSelections(check, []net.IPNet{*route.Dst}, nil)
			if err != nil {
				return err
			}
			if indices[raw] != route.LinkIndex {
				return fmt.Errorf("scoped route uses a different receiver")
			}
		}
	}
	return nil
}
