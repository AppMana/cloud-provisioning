package main

import (
	"net"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestAdvertisementLegacyPeerRejectsShadowedTraffic(t *testing.T) {
	root, _ := wgtypes.GeneratePrivateKey()
	worker, _ := wgtypes.GeneratePrivateKey()
	other, _ := wgtypes.GeneratePrivateKey()
	cidr := func(s string) net.IPNet {
		_, p, e := net.ParseCIDR(s)
		if e != nil {
			t.Fatal(e)
		}
		return *p
	}
	now := time.Now()
	for _, tc := range []struct {
		name, shadow string
		bad          bool
	}{
		{"healthy", "", false}, {"unrelated", "172.31.0.0/24", false},
		{"less-specific", "172.16.0.0/12", false}, {"equal", "172.30.0.0/24", true},
		{"more-specific", "172.30.0.128/25", true}, {"ipv6-more-specific", "fd30::80/121", true},
		{"stale", "", true}, {"wrong-root", "", true}, {"wrong-mark", "", true}, {"missing-permission", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowed := []string{"172.30.0.0/24", "fd30::/64"}
			group := receiveAdvertisementGroup{config: config{fwmark: 518}, peer: tunnel.PeerSpec{PublicKey: worker.PublicKey().String(), WGAllowedIPs: allowed}, destinations: []net.IPNet{cidr(allowed[0]), cidr(allowed[1])}}
			device := &wgtypes.Device{PublicKey: root.PublicKey(), FirewallMark: 518, Peers: []wgtypes.Peer{{PublicKey: worker.PublicKey(), AllowedIPs: group.destinations, Endpoint: &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 51820}, LastHandshakeTime: now.Add(-time.Second)}}}
			if tc.shadow != "" {
				device.Peers = append(device.Peers, wgtypes.Peer{PublicKey: other.PublicKey(), AllowedIPs: []net.IPNet{cidr(tc.shadow)}})
			}
			switch tc.name {
			case "stale":
				device.Peers[0].LastHandshakeTime = now.Add(-2 * receiveHandshakeMaxAge)
			case "wrong-root":
				device.PublicKey = other.PublicKey()
			case "wrong-mark":
				device.FirewallMark++
			case "missing-permission":
				device.Peers[0].AllowedIPs = device.Peers[0].AllowedIPs[:1]
			}
			err := verifyAdvertisementLegacyPeer(device, group, root.PublicKey().String(), now)
			if (err != nil) != tc.bad {
				t.Fatalf("rejection=%v, want %v: %v", err != nil, tc.bad, err)
			}
		})
	}
}

func TestReceiveAdvertisementPlanKeepsUnrelatedWorker(t *testing.T) {
	site, _ := wgtypes.GeneratePrivateKey()
	worker, _ := wgtypes.GeneratePrivateKey()
	other, _ := wgtypes.GeneratePrivateKey()
	cfg := config{nodeName: "site"}
	data := map[string][]byte{
		tunnel.NodePublicKeyPrefix + "site":     []byte(site.PublicKey().String()),
		tunnel.NodeTunnelAddressPrefix + "site": []byte("10.88.0.1/32"),
		tunnel.NodeAddressesPrefix + "site":     []byte("10.10.0.1"),
		tunnel.PeerPublicKeyPrefix + "worker":   []byte(worker.PublicKey().String()),
		tunnel.PeerAllowedIPsPrefix + "worker":  []byte("172.30.0.0/24"),
		tunnel.PeerRouteHostsPrefix + "worker":  []byte("172.30.0.1"),
		tunnel.PeerPublicKeyPrefix + "other":    []byte(other.PublicKey().String()),
		tunnel.PeerAllowedIPsPrefix + "other":   []byte("172.31.0.0/24"),
		tunnel.PeerRouteHostsPrefix + "other":   []byte("172.31.0.1"),
	}
	peers, err := tunnel.SitePeers(data)
	if err != nil {
		t.Fatal(err)
	}
	plan := tunnel.ReceiveTransitionPlan{MachineUID: "worker", MeshUID: "mesh", RootPublicKey: worker.PublicKey().String(), SiteDesired: tunnel.PeerListDoc{Peers: peers}, Sites: map[string]tunnel.ReceiveTransitionSite{"site": {NodeName: "site", NodeUID: "site-uid", RouteSources: []string{"10.10.0.1"}, Legacy: &tunnel.ReceiveSiteLegacy{}}}}
	intent := tunnel.ReceiveJournalRoutes{Plans: map[string]tunnel.ReceiveTransitionPlan{"worker": plan}, Legacy: map[string]tunnel.ReceiveTransitionPlan{"worker": plan}}
	saved := &siteForwardingCache{NodeName: "site", MeshUID: "mesh", Data: data}
	groups, err := planReceiveAdvertisements(cfg, saved, "site-uid", intent)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("lost unrelated worker: %+v", groups)
	}
	for _, g := range groups {
		if !g.legacy || len(g.routes) != 2 {
			t.Fatalf("unexpected path: %+v", g)
		}
	}
	// The committed generation selects the route even if the old cache
	// still describes the previous relay preference.
	sitePlan := plan.Sites["site"]
	sitePlan.Forward = &tunnel.ReceiveSitePath{Transit: &tunnel.TransitSpec{Via: "10.10.0.2"}}
	plan.Sites["site"] = sitePlan
	intent.Plans["worker"] = plan
	delete(intent.Legacy, "worker")
	groups, err = planReceiveAdvertisements(cfg, saved, "site-uid", intent)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.machine == "worker" && (g.legacy || g.config.receiveRelayRoutes["172.30.0.0/24"] != "10.10.0.2") {
			t.Fatalf("lost committed relay: %+v", g)
		}
		if g.machine == "" && !g.legacy {
			t.Fatal("changed unrelated worker")
		}
	}
	sitePlan.Forward = &tunnel.ReceiveSitePath{}
	plan.Sites["site"] = sitePlan
	intent.Plans["worker"] = plan
	if _, err := planReceiveAdvertisements(cfg, saved, "site-uid", intent); err == nil {
		t.Fatal("accepted absent receiver selection")
	}
	selection := tunnel.ReceiveRouteSelection{Interface: "receiver"}
	intent.Receivers = map[string]map[string]tunnel.ReceiveRouteSelection{"site-uid": {"172.30.0.0/24": selection, "172.30.0.1/32": selection}}
	groups, err = planReceiveAdvertisements(cfg, saved, "site-uid", intent)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.machine == "worker" && (g.legacy || len(g.config.receiveRoutes) != 2) {
			t.Fatal("lost receiver selection")
		}
	}
	data[tunnel.PeerAllowedIPsPrefix+"other"] = []byte("172.30.0.128/25")
	if _, err := planReceiveAdvertisements(cfg, saved, "site-uid", intent); err == nil {
		t.Fatal("accepted overlapping worker destinations")
	}
	data[tunnel.PeerAllowedIPsPrefix+"other"] = []byte("172.31.0.0/24")
	saved.MeshUID = "replacement"
	if _, err := planReceiveAdvertisements(cfg, saved, "site-uid", intent); err == nil {
		t.Fatal("accepted replacement mesh")
	}
}
