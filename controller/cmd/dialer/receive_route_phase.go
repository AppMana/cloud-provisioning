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

// receiveLinuxPhase implements generation handover operations.
// Admission, durable checkpointing and the host lock belong to executeReceivePhase.
// own contains admitted route source addresses; allocated contains the distinct
// tunnel allocation for the stable address owner. Neither comes from kernel discovery.
func receiveLinuxPhase(ctx context.Context, devices receiverDevices, cfg config, key wgtypes.Key, own, allocated []string, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if view.Complete {
		return fmt.Errorf("native receiver route backend does not implement this phase")
	}
	switch view.Round.Phase {
	case handover.Prepare, handover.Switch, handover.Drain, handover.Retire, handover.Abort:
	default:
		return fmt.Errorf("unsupported native receiver phase")
	}
	if devices == nil || key == (wgtypes.Key{}) || nodeUID == "" {
		return fmt.Errorf("native receiver route phase requires local identity")
	}
	expected, err := tunnel.ReceiveTransitionSpec(plan, view.Round.ClusterUID, view.Round.PlanHash)
	if err != nil {
		return err
	}
	expected.Phase = view.Round.Phase
	if view.RollbackRequested && view.Round.Phase != handover.Abort {
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
	cfg.receiveRoutes = map[string]receiveRouteSelection{}
	for raw, selected := range intent.Receivers[nodeUID] {
		cfg.receiveRoutes[raw] = selected
	}
	cfg.receiveSourceRoutes = map[string]receiveRouteSelection{}
	cfg.receiveSourceCandidates = map[string]receiveRouteSelection{}
	cfg.receiveRelayRoutes = map[string]string{}
	allLegacyRelays := map[string]string{}
	allForwardRelays := map[string]string{}
	cfg.receiveRouteScope = map[string]bool{}
	desired := map[string]bool{}
	for raw := range cfg.receiveRoutes {
		desired[raw] = true
	}
	legacyPeers := map[string]tunnel.PeerSpec{}
	targetLegacyPeers := map[string]tunnel.PeerSpec{}
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
		var relay *tunnel.TransitSpec
		requireLegacyPeer := true
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
			for public, site := range p.Sites {
				if site.NodeUID == nodeUID {
					if site.Forward != nil {
						_, legacySelected := intent.Legacy[machineUID]
						for _, raw := range site.Forward.Tunnel.Hosts {
							selection, found := routes[nodeUID][raw]
							if !found {
								return fmt.Errorf("source exception lacks its committed receiver")
							}
							cfg.receiveSourceCandidates[raw] = selection
							if !legacySelected {
								cfg.receiveSourceRoutes[raw] = selection
							}
						}
						if site.Forward.Transit != nil {
							for raw := range routes[nodeUID] {
								_, dst, err := net.ParseCIDR(raw)
								if err != nil || site.Forward.Transit.Gateway(dst.IP) == nil {
									return fmt.Errorf("desired relay lacks destination gateway")
								}
								allForwardRelays[raw] = site.Forward.Transit.Gateway(dst.IP).String()
								if !legacySelected {
									cfg.receiveRelayRoutes[raw] = allForwardRelays[raw]
									delete(cfg.receiveRoutes, raw)
								}
							}
						}
					}
					peer, err := receiveSiteLegacyPeer(p, public)
					if err != nil {
						return err
					}
					if site.Legacy != nil && site.Legacy.Transit != nil {
						if site.Legacy.Tunnel == nil || intent.Sources == nil {
							return fmt.Errorf("relayed phase requires committed source exceptions")
						}
						relay = site.Legacy.Transit
						requireLegacyPeer = len(site.Legacy.Tunnel.Hosts) != 0
						if site.Forward == nil {
							if err := validateReceiveSiteSources(p, public); err != nil {
								return err
							}
						}
					}
					old = append(old, peer)
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
				if relay != nil {
					_, dst, err := net.ParseCIDR(raw)
					if err != nil || relay.Gateway(dst.IP) == nil {
						return fmt.Errorf("legacy relay lacks a destination gateway")
					}
					allLegacyRelays[raw] = relay.Gateway(dst.IP).String()
					if legacy {
						cfg.receiveRelayRoutes[raw] = allLegacyRelays[raw]
					}
				}
				if legacy {
					desired[raw] = true
				}
			}
			if machineUID == plan.MachineUID {
				targetLegacyPeers[peer.PublicKey] = peer
			}
			if requireLegacyPeer && (legacy || (machineUID == plan.MachineUID && view.Round.Phase != handover.Retire)) {
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
	if len(expectedRoutes) != len(intent.Receivers[nodeUID]) || (len(expectedRoutes) != 0 && !reflect.DeepEqual(expectedRoutes, intent.Receivers[nodeUID])) {
		return fmt.Errorf("native receiver selections differ from admitted plans")
	}
	_, targetLegacy := intent.Legacy[plan.MachineUID]
	wantLegacy := view.RollbackRequested || view.Round.Phase == handover.Prepare || view.Round.Phase == handover.Abort
	if targetLegacy != wantLegacy {
		return fmt.Errorf("native route direction differs from committed phase")
	}
	if view.Round.Phase == handover.Switch {
		if err := verifyReceiveRelayDependencies(intent, nodeUID, plan.MachineUID, view, time.Now()); err != nil {
			return err
		}
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
	sourceAddresses, sourceHosts, admittedSourceHosts, err := receivePhaseSources(intent, nodeUID, allocated)
	if err != nil {
		return err
	}
	if view.Round.Phase != handover.Switch {
		if err := verifyTunnelSourceRoutes(cfg, sourceAddresses, sourceHosts); err != nil {
			return err
		}
	}
	allocation := stableAddressOwnerRequest{MeshUID: plan.MeshUID, NodeUID: nodeUID, LegacyInterface: cfg.iface, RootPublicKey: key.PublicKey().String(), Addresses: allocated}
	if view.Round.Phase == handover.Prepare {
		if err := verifyReceiveRouteInstallation(cfg, hosts, nil, own); err != nil {
			return err
		}
		if _, err := prepareStableAddressOwner(devices, allocation); err != nil {
			return err
		}
		if err := prepareReceivePhasePaths(devices, cfg, key, plan, nodeUID); err != nil {
			return err
		}
	} else {
		if _, err := verifyStableAddressOwner(allocation); err != nil && !(view.Round.Phase == handover.Abort && isLinkNotFound(err)) {
			return err
		}
	}
	retiringReceivers := view.Round.Phase == handover.Abort || (view.Round.Phase == handover.Retire && targetLegacy)
	if !retiringReceivers {
		if err := verifyReceivePhasePaths(devices, cfg, key, plan, nodeUID); err != nil {
			return err
		}
	}
	if view.Round.Phase == handover.Switch {
		if err := preflightReceiveRoutePhase(cfg, allRoutes[nodeUID], own, allLegacyRelays, allForwardRelays); err != nil {
			return err
		}
		staged, stagedReceivers, err := preflightReceiveSourceSwitchPlan(cfg, sourceAddresses, sourceHosts, admittedSourceHosts)
		if err != nil {
			return err
		}
		// Add rollback exceptions before changing egress, but retain forward
		// exceptions until the replacement egress is installed.
		stageCfg := cfg
		stageCfg.receiveSourceRoutes = stagedReceivers
		if err := reconcileTunnelSourceRoutes(stageCfg, sourceAddresses, staged); err != nil {
			return err
		}
		if err := installRoutes(cfg, hosts, nil, nil, nil, nil, own); err != nil {
			return err
		}
		if err := reconcileTunnelSourceRoutes(cfg, sourceAddresses, sourceHosts); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyTunnelSourceRoutes(cfg, sourceAddresses, sourceHosts); err != nil {
		return err
	}
	// Deletion never repairs routes to make its safety checks pass.
	if err := verifyReceiveRouteInstallation(cfg, hosts, nil, own); err != nil {
		return err
	}
	if retiringReceivers {
		selections, err := receivePhaseSelections(key, plan, nodeUID)
		if err != nil {
			return err
		}
		for _, selection := range selections {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := retireReceiveDevice(devices, selection); err != nil {
				return err
			}
		}
		for _, selection := range selections {
			if _, err := netlink.LinkByName(selection.Interface); !isLinkNotFound(err) {
				return fmt.Errorf("retired receiver is still present: %v", err)
			}
		}
	} else if view.Round.Phase == handover.Retire {
		for _, peer := range targetLegacyPeers {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := withdrawLegacyReceivePeer(devices, cfg.iface, key.PublicKey().String(), peer.PublicKey, peer.WGAllowedIPs); err != nil {
				return err
			}
		}
		current, err := devices.Device(cfg.iface)
		if err != nil {
			return err
		}
		for _, peer := range current.Peers {
			if _, retired := targetLegacyPeers[peer.PublicKey.String()]; retired {
				return fmt.Errorf("retired legacy peer is still present")
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := verifyStableAddressOwner(allocation); err != nil && !(view.Round.Phase == handover.Abort && isLinkNotFound(err)) {
		return err
	}
	return verifyReceiveRouteInstallation(cfg, hosts, nil, own)
}

// A cloud participant can have retained receive paths with no desired egress.
// Verify those members too before the execution boundary can issue their receipts.
func receivePhaseSelections(key wgtypes.Key, plan tunnel.ReceiveTransitionPlan, nodeUID string) ([]receiveRouteSelection, error) {
	var selections []receiveRouteSelection
	if nodeUID == plan.CloudNodeUID {
		var base tunnel.PeerListDoc
		if err := json.Unmarshal(plan.Base, &base); err != nil {
			return nil, err
		}
		paths, err := tunnel.PlanReceiveHandover(plan.MeshUID, base, plan.Desired, plan.Native)
		if err != nil {
			return nil, err
		}
		for _, path := range paths.Receivers {
			binding := plan.Bindings[path.PublicKey]
			if binding.PublicKey != receiverKey(key, plan.MeshUID, path.PublicKey).PublicKey().String() {
				return nil, fmt.Errorf("receiver phase key differs from local derivation")
			}
			selections = append(selections, receiveRouteSelection{Interface: binding.Interface, Alias: receiverAlias(plan.MeshUID, path.PublicKey), PublicKey: binding.PublicKey, PeerPublicKey: path.PublicKey, AllowedSources: path.AllowedSources})
		}
	} else {
		site, found := plan.Sites[key.PublicKey().String()]
		if !found || site.NodeUID != nodeUID {
			return nil, fmt.Errorf("native site receive identity is missing")
		}
		routes, err := tunnel.PlanReceiveRoutes(plan)
		if err != nil {
			return nil, err
		}
		for _, selection := range routes[nodeUID] {
			selections = append(selections, selection)
			break
		}
	}
	if len(selections) == 0 {
		return nil, fmt.Errorf("native phase has no local receive paths")
	}
	return selections, nil
}

func verifyReceivePhasePaths(devices receiverDevices, cfg config, key wgtypes.Key, plan tunnel.ReceiveTransitionPlan, nodeUID string) error {
	selections, err := receivePhaseSelections(key, plan, nodeUID)
	if err != nil {
		return err
	}
	if nodeUID != plan.CloudNodeUID {
		site := plan.Sites[key.PublicKey().String()]
		handshake, err := siteReceiveHandshake(devices, key, site.Client, cfg.fwmark, time.Now())
		if err != nil || handshake.IsZero() {
			return fmt.Errorf("native site receive transport is not ready: %v", err)
		}
	}
	for _, selection := range selections {
		if nodeUID == plan.CloudNodeUID {
			live, err := devices.Device(selection.Interface)
			if err != nil {
				return err
			}
			if live.ListenPort != plan.Bindings[selection.PeerPublicKey].ListenPort {
				return fmt.Errorf("receiver phase listener differs from reserved port")
			}
		}
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

func prepareReceivePhasePaths(devices receiverDevices, cfg config, key wgtypes.Key, plan tunnel.ReceiveTransitionPlan, nodeUID string) error {
	mtu := cfg.mtu
	if mtu == 0 {
		mtu = underlayMTU()
	}
	if nodeUID == plan.CloudNodeUID {
		var base tunnel.PeerListDoc
		if err := json.Unmarshal(plan.Base, &base); err != nil {
			return err
		}
		bindings, err := prepareReceivePaths(devices, plan.MeshUID, key, base, plan.Desired, plan.Native, mtu, cfg.fwmark, plan.Ports)
		if err != nil {
			return err
		}
		actual := map[string]tunnel.ReceiveBinding{}
		for _, binding := range bindings {
			actual[binding.PeerPublicKey] = binding
		}
		if !reflect.DeepEqual(actual, plan.Bindings) {
			return fmt.Errorf("prepared receivers differ from committed bindings")
		}
	} else {
		site, found := plan.Sites[key.PublicKey().String()]
		if !found || site.NodeUID != nodeUID {
			return fmt.Errorf("missing site preparation identity")
		}
		client, err := prepareSiteReceiveClient(devices, plan.MeshUID, key, plan.RootPublicKey, plan.Bindings[key.PublicKey().String()], plan.SiteDesired, plan.SiteDesired, mtu, cfg.fwmark)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(client, site.Client) {
			return fmt.Errorf("prepared site receiver differs from committed binding")
		}
	}
	return nil
}

// Only the admitted legacy device or the exact prepared receiver may already
// own a scoped destination. Reject foreign routes and more-specific shadows
// before installRoutes can replace anything. Unrelated destinations survive.
func preflightReceiveRoutePhase(cfg config, receivers map[string]receiveRouteSelection, own []string, admittedRelays ...map[string]string) error {
	legacy, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return err
	}
	if legacy.Type() != "wireguard" {
		return fmt.Errorf("legacy route device is not WireGuard")
	}
	if len(admittedRelays) > 2 {
		return fmt.Errorf("too many admitted relay generations")
	}
	if len(admittedRelays) == 0 {
		admittedRelays = []map[string]string{cfg.receiveRelayRoutes}
	}
	relayIndices := make([]map[string]int, len(admittedRelays))
	for i, relays := range admittedRelays {
		relayCfg := cfg
		relayCfg.receiveRelayRoutes, relayCfg.receiveRoutes = relays, nil
		var hosts []net.IPNet
		for raw := range relays {
			_, dst, err := net.ParseCIDR(raw)
			if err != nil {
				return err
			}
			hosts = append(hosts, *dst)
		}
		indices, err := validateReceiveRelayRoutes(relayCfg, hosts, nil)
		if err != nil {
			return err
		}
		relayIndices[i] = indices
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
			if seen[raw] || route.Type != unix.RTN_UNICAST || route.Protocol != unix.RTPROT_BOOT || route.Priority != receiveRoutePriority(route.Dst) || len(route.MultiPath) != 0 || route.Via != nil || route.Encap != nil || route.NewDst != nil || route.Tos != 0 || !route.Src.Equal(transitSource(route.Dst.IP, own, local)) {
				return fmt.Errorf("scoped destination has conflicting route ownership: %s protocol=%d priority=%d source=%s expected-source=%s", raw, route.Protocol, route.Priority, route.Src, transitSource(route.Dst.IP, own, local))
			}
			seen[raw] = true
			if len(route.Gw) != 0 {
				admitted := false
				for i, relays := range admittedRelays {
					admitted = admitted || (route.LinkIndex == relayIndices[i][raw] && route.Gw.Equal(net.ParseIP(relays[raw])))
				}
				if !admitted {
					return fmt.Errorf("scoped route uses an unadmitted relay")
				}
				continue
			}
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

// receiveDirectSiteLegacyPeer rejects source expansion that the committed legacy
// cloud peer cannot accept. A local peer and a handshake alone cannot prove this.
// This is a necessary check, not proof of source-sensitive site forwarding.
// Source transfers and relayed sites require a separate admitted legacy routing
// plan; the direct backend must reject them before any native mutation.
func receiveDirectSiteLegacyPeer(p tunnel.ReceiveTransitionPlan, public string) (tunnel.PeerSpec, error) {
	if legacy := p.Sites[public].Legacy; legacy != nil && legacy.Transit != nil {
		return tunnel.PeerSpec{}, fmt.Errorf("site requires a relayed legacy path")
	}
	return receiveSiteLegacyPeer(p, public)
}

func receiveSiteLegacyPeer(p tunnel.ReceiveTransitionPlan, public string) (tunnel.PeerSpec, error) {
	fail := func() (tunnel.PeerSpec, error) {
		return tunnel.PeerSpec{}, fmt.Errorf("site requires an admitted legacy source path")
	}
	var base tunnel.PeerListDoc
	if err := json.Unmarshal(p.Base, &base); err != nil {
		return tunnel.PeerSpec{}, err
	}
	if legacy := p.Sites[public].Legacy; legacy != nil {
		for _, peer := range p.SiteDesired.Peers {
			if peer.PublicKey == p.RootPublicKey {
				if err := tunnel.ValidateReceiveSiteLegacy(*legacy, public, base, peer); err != nil {
					return tunnel.PeerSpec{}, err
				}
				return peer, nil
			}
		}
		return fail()
	}
	var sources []netip.Prefix
	for _, peer := range base.Peers {
		if peer.PublicKey != public || peer.Remote {
			continue
		}
		for _, raw := range peer.WGAllowedIPs {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				return fail()
			}
			sources = append(sources, prefix)
		}
	}
	if len(sources) == 0 {
		return fail()
	}
	for _, peer := range p.Desired.Peers {
		if peer.PublicKey != public || peer.Remote {
			continue
		}
		for _, raw := range peer.WGAllowedIPs {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				return fail()
			}
			covered := false
			for _, previous := range sources {
				covered = covered || (previous.Bits() <= prefix.Bits() && previous.Contains(prefix.Addr()))
			}
			if !covered {
				return fail()
			}
		}
	}
	for _, peer := range p.SiteDesired.Peers {
		if peer.PublicKey == p.RootPublicKey {
			return peer, nil
		}
	}
	return fail()
}
