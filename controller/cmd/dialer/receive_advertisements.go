package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type receiveAdvertisementGroup struct {
	machine      string
	peer         tunnel.PeerSpec
	config       config
	own          []string
	destinations []net.IPNet
	routes       []transitRoute
	legacy       bool
}

// Keep the last admitted advertisement graph and preference, but prove its
// current forwarding selection. Journals replace per-worker egress; unrelated
// workers retain the exact acknowledged legacy or relay selection in the cache.
func planReceiveAdvertisements(cfg config, saved *siteForwardingCache, nodeUID string, intent tunnel.ReceiveJournalRoutes) ([]receiveAdvertisementGroup, error) {
	if saved == nil || saved.Transit || saved.NodeName != cfg.nodeName || saved.MeshUID == "" || len(intent.Plans) == 0 {
		return nil, fmt.Errorf("receiver advertisements require an endpoint forwarding snapshot")
	}
	peers, err := tunnel.SitePeers(saved.Data)
	if err != nil {
		return nil, err
	}
	role, oldTransit, err := tunnel.SiteForwardingRole(saved.Data, cfg.nodeName, saved.NotReady)
	if err != nil {
		return nil, err
	}
	byPublic := map[string]string{}
	for machine, plan := range intent.Plans {
		if plan.MeshUID != saved.MeshUID {
			return nil, fmt.Errorf("advertisement journal belongs to another mesh")
		}
		if _, exists := byPublic[plan.RootPublicKey]; exists {
			return nil, fmt.Errorf("ambiguous advertisement worker identity")
		}
		byPublic[plan.RootPublicKey] = machine
	}
	seen := map[string]bool{}
	owners := map[netip.Prefix]string{}
	var result []receiveAdvertisementGroup
	for _, peer := range peers {
		if !peer.Remote {
			continue
		}
		for _, withdrawn := range intent.Withdrawn {
			if peer.PublicKey == withdrawn {
				return nil, fmt.Errorf("advertisement cache still contains a withdrawn worker")
			}
		}
		g := receiveAdvertisementGroup{peer: peer, config: cfg, own: tunnel.ReceiveSiteRouteSources(saved.Data, cfg.nodeName), legacy: true}
		g.config.receiveRouteScope = map[string]bool{}
		g.config.receiveRoutes = map[string]receiveRouteSelection{}
		g.config.receiveRelayRoutes = map[string]string{}
		var relay *tunnel.TransitSpec
		if machine, found := byPublic[peer.PublicKey]; found {
			g.machine = machine
			seen[machine] = true
			plan := intent.Plans[machine]
			active := plan
			_, g.legacy = intent.Legacy[machine]
			previous := g.legacy
			if plan.PreviousReceiver != nil {
				view, found := intent.Views[machine]
				if !found || g.legacy {
					return nil, fmt.Errorf("successor advertisement lacks receiver selection")
				}
				active, _, err = plan.SelectedReceiveTransport(view)
				if err != nil {
					return nil, err
				}
				previous = active.ReceiverGeneration == plan.PreviousReceiver.Plan.ReceiverGeneration
			}
			var site tunnel.ReceiveTransitionSite
			for _, candidate := range active.Sites {
				if candidate.NodeUID == nodeUID {
					site = candidate
				}
			}
			if site.NodeName != cfg.nodeName || !slices.Equal(site.RouteSources, g.own) {
				return nil, fmt.Errorf("advertising endpoint differs from journal source identity")
			}
			matched := false
			var selected *tunnel.PeerSpec
			for generation, document := range []tunnel.PeerListDoc{plan.PreviousSitePeers(), plan.SiteDesired} {
				for _, candidate := range document.Peers {
					if candidate.PublicKey != peer.PublicKey {
						continue
					}
					matched = matched || (sameAdvertisementSet(candidate.WGAllowedIPs, peer.WGAllowedIPs) && sameAdvertisementSet(candidate.AllRouteHosts(), peer.AllRouteHosts()))
					if (generation == 0) == previous {
						copy := candidate
						selected = &copy
					}
				}
			}
			if !matched || selected == nil {
				return nil, fmt.Errorf("advertisement graph differs from committed worker destinations")
			}
			// The cache must belong to one committed generation, but only the
			// serving generation can authorize advertisements. This preserves
			// rollback destinations without prematurely announcing new ranges.
			peer, g.peer = *selected, *selected
			path := site.Forward
			if g.legacy {
				path = site.Legacy
			}
			if path == nil {
				return nil, fmt.Errorf("advertisement path is missing")
			}
			relay = path.Transit
		} else if role == "relayed" {
			for name, public := range saved.Data {
				if strings.HasPrefix(name, tunnel.PeerPublicKeyPrefix) && strings.TrimSpace(string(public)) == peer.PublicKey && saved.PeerAcked[strings.TrimPrefix(name, tunnel.PeerPublicKeyPrefix)] {
					relay = oldTransit
				}
			}
		}
		prefixes := map[string]bool{}
		for _, host := range peer.AllRouteHosts() {
			prefixes[tunnel.HostCIDR(host)] = true
		}
		for _, raw := range peer.WGAllowedIPs {
			prefixes[raw] = true
		}
		for raw := range prefixes {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil || prefix != prefix.Masked() || prefix.Bits() == 0 || !prefix.Addr().IsGlobalUnicast() || prefix.Addr().Is4In6() {
				return nil, fmt.Errorf("invalid advertisement destination")
			}
			for other, owner := range owners {
				if owner != peer.PublicKey && prefix.Overlaps(other) {
					return nil, fmt.Errorf("advertisement has ambiguous worker ownership")
				}
			}
			owners[prefix] = peer.PublicKey
			_, dst, _ := net.ParseCIDR(raw)
			g.destinations = append(g.destinations, *dst)
			g.routes = append(g.routes, transitRoute{prefix: raw, med: endpointRank(saved.Data, cfg.nodeName)})
			g.config.receiveRouteScope[raw] = true
			if relay != nil {
				gateway := relay.Gateway(dst.IP)
				if gateway == nil {
					return nil, fmt.Errorf("advertisement relay lacks a gateway")
				}
				g.config.receiveRelayRoutes[raw] = gateway.String()
			} else if !g.legacy {
				selection, found := intent.Receivers[nodeUID][raw]
				if !found {
					return nil, fmt.Errorf("advertisement lacks its committed receiver")
				}
				g.config.receiveRoutes[raw] = selection
			}
		}
		g.legacy = g.legacy && relay == nil
		result = append(result, g)
	}
	for machine, plan := range intent.Plans {
		for _, site := range plan.Sites {
			if site.NodeUID == nodeUID && !seen[machine] {
				return nil, fmt.Errorf("journal worker absent from advertisement snapshot")
			}
		}
	}
	return result, nil
}

func sameAdvertisementSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func verifyAdvertisementLegacyPeer(device *wgtypes.Device, group receiveAdvertisementGroup, public string, now time.Time) error {
	if device == nil || device.PublicKey.String() != public || device.FirewallMark != group.config.fwmark {
		return fmt.Errorf("advertisement legacy device identity differs")
	}
	var selected *wgtypes.Peer
	for i := range device.Peers {
		if device.Peers[i].PublicKey.String() == group.peer.PublicKey {
			selected = &device.Peers[i]
		}
	}
	if selected == nil || !receiveSourcesMatch(group.peer.WGAllowedIPs, selected.AllowedIPs) || selected.Endpoint == nil || selected.LastHandshakeTime.IsZero() || selected.LastHandshakeTime.After(now) || now.Sub(selected.LastHandshakeTime) > receiveHandshakeMaxAge {
		return fmt.Errorf("advertisement legacy peer lacks current authenticated permissions")
	}
	for _, dst := range group.destinations {
		prefix, _ := netip.ParsePrefix(dst.String())
		coverage := -1
		for _, raw := range selected.AllowedIPs {
			allowed, _ := netip.ParsePrefix(raw.String())
			if allowed.Addr().BitLen() == prefix.Addr().BitLen() && allowed.Bits() <= prefix.Bits() && allowed.Contains(prefix.Addr()) && allowed.Bits() > coverage {
				coverage = allowed.Bits()
			}
		}
		if coverage < 0 {
			return fmt.Errorf("advertisement not covered by its legacy peer")
		}
		for _, peer := range device.Peers {
			if peer.PublicKey == selected.PublicKey {
				continue
			}
			for _, raw := range peer.AllowedIPs {
				allowed, _ := netip.ParsePrefix(raw.String())
				if allowed.Addr().BitLen() == prefix.Addr().BitLen() && allowed.Overlaps(prefix) && allowed.Bits() >= coverage {
					return fmt.Errorf("another peer shadows the advertised destination")
				}
			}
		}
	}
	return nil
}

// Called inside the phase executor's host lock, after native verification.
// Never replay forwarding caches here: only announce routes whose current
// kernel selection has been read back. An unavailable unrelated worker must
// not prevent restoring advertisements for healthy workers after a restart.
func reconcileReceiveAdvertisements(ctx context.Context, devices receiverDevices, cfg config, speaker *transitSpeaker, target, nodeUID string, intent tunnel.ReceiveJournalRoutes) error {
	if speaker == nil || cfg.secretName == "" {
		return nil
	}
	cached, saved, err := readTransitAdvertisements(cfg, speaker)
	if err != nil {
		return err
	}
	groups, err := planReceiveAdvertisements(cfg, saved, nodeUID, intent)
	if err != nil {
		return err
	}
	var routes []transitRoute
	var targetErr error
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return err
		}
		var verifyErr error
		if group.legacy {
			device, err := devices.Device(cfg.iface)
			if err != nil {
				verifyErr = err
			} else {
				verifyErr = verifyAdvertisementLegacyPeer(device, group, saved.PublicKey, time.Now())
			}
		}
		if verifyErr == nil {
			verifyErr = verifyReceiveRouteInstallation(group.config, group.destinations, nil, group.own)
		}
		if verifyErr != nil {
			if group.machine == target {
				targetErr = fmt.Errorf("handover advertisement verification: %w", verifyErr)
			}
			fmt.Fprintf(os.Stderr, "withholding unverifiable worker advertisements: %v\n", verifyErr)
			continue
		}
		routes = append(routes, group.routes...)
	}
	if routes == nil {
		routes = []transitRoute{}
	}
	if err := speaker.reconcile(ctx, cached.Peers, routes); err != nil {
		return err
	}
	return targetErr
}
