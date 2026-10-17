package main

import (
	"encoding/json"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"net/netip"
	"testing"
)

func TestReceiveSiteSourcesRejectBareReceiver(t *testing.T) {
	local, _ := wgtypes.GeneratePrivateKey()
	relay, _ := wgtypes.GeneratePrivateKey()
	public := local.PublicKey().String()
	peer := tunnel.PeerSpec{PublicKey: public, WGAllowedIPs: []string{"10.100.0.2/32"}}
	owner := tunnel.PeerSpec{PublicKey: relay.PublicKey().String(), WGAllowedIPs: []string{"10.90.0.0/24"}}
	base, _ := json.Marshal(tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{peer, owner}})
	p := tunnel.ReceiveTransitionPlan{MeshUID: "mesh", Base: base, Desired: tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{peer, owner}}, Sites: map[string]tunnel.ReceiveTransitionSite{public: {Legacy: &tunnel.ReceiveSiteLegacy{Sources: []string{"10.90.0.10/32"}, Tunnel: &tunnel.ReceiveSiteTunnel{Addresses: []string{"10.100.0.2/24"}}}}}}
	if err := validateReceiveSiteSources(p, public); err == nil {
		t.Fatal("bare receiver accepted workload source")
	}
	peer.WGAllowedIPs = append(peer.WGAllowedIPs, "10.90.0.0/24")
	p.Desired.Peers = []tunnel.PeerSpec{peer}
	if err := validateReceiveSiteSources(p, public); err != nil {
		t.Fatal("new owner rejected", err)
	}
	site := p.Sites[public]
	site.Legacy.Tunnel.Addresses = []string{"10.100.0.3/24"}
	if err := validateReceiveSiteSources(p, public); err == nil {
		t.Fatal("unaccepted tunnel allocation admitted")
	}
}

func TestReceivePhaseSourcesBindStableAllocation(t *testing.T) {
	root, _ := wgtypes.GeneratePrivateKey()
	site, _ := wgtypes.GeneratePrivateKey()
	public := site.PublicKey().String()
	peer := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: public, WGAllowedIPs: []string{"10.90.0.0/24"}}}}
	base, _ := json.Marshal(peer)
	prep, err := tunnel.PlanReceiveHandover("mesh", peer, peer, nil)
	if err != nil {
		t.Fatal(err)
	}
	binding := tunnel.ReceiveBinding{Interface: prep.Receivers[0].Interface, PeerPublicKey: public, PublicKey: receiverKey(root, "mesh", public).PublicKey().String(), ListenPort: 51821}
	cloud := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: root.PublicKey().String(), Remote: true, Endpoint: "198.51.100.1:51820", WGAllowedIPs: []string{"172.29.0.1/32"}}}}
	client, err := tunnel.PlanSiteReceiveClient("mesh", public, root.PublicKey().String(), binding, cloud, cloud)
	if err != nil {
		t.Fatal(err)
	}
	p := tunnel.ReceiveTransitionPlan{Version: 1, Namespace: "test", MeshName: "mesh", CloudNodeName: "cloud", MachineUID: "worker", MeshUID: "mesh", CloudNodeUID: "cloud", RootPublicKey: root.PublicKey().String(), Base: base, Desired: peer, SiteDesired: cloud, Native: []netip.Prefix{}, Ports: map[string]int{public: 51821}, Bindings: map[string]tunnel.ReceiveBinding{public: binding}, Sites: map[string]tunnel.ReceiveTransitionSite{public: {NodeName: "site", NodeUID: "site", Client: client, Legacy: &tunnel.ReceiveSiteLegacy{Sources: []string{"10.90.0.0/24"}, PeerPublicKey: public, Tunnel: &tunnel.ReceiveSiteTunnel{Addresses: []string{"10.90.0.2/24"}, Hosts: []string{}}}}}}
	routes, err := tunnel.PlanReceiveRoutes(p)
	if err != nil {
		t.Fatal(err)
	}
	intent := tunnel.ReceiveJournalRoutes{Plans: map[string]tunnel.ReceiveTransitionPlan{"worker": p}, Legacy: map[string]tunnel.ReceiveTransitionPlan{}, Receivers: routes}
	intent.Sources, err = tunnel.PlanReceiveSourceRoutes(intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := receivePhaseSources(intent, "site", []string{"10.90.0.2/32"}); err != nil {
		t.Fatal("equivalent stable allocation rejected", err)
	}
	if _, _, _, err := receivePhaseSources(intent, "site", []string{"10.90.0.3/32"}); err == nil {
		t.Fatal("different stable allocation admitted")
	}
	original := intent.Sources
	intent.Sources = nil
	if _, _, _, err := receivePhaseSources(intent, "site", []string{"10.90.0.2/32"}); err == nil {
		t.Fatal("omitted committed projection admitted")
	}
	intent.Sources = original
	changed := intent.Sources["site"]
	changed.Hosts = []string{"172.29.0.1/32"}
	intent.Sources["site"] = changed
	if _, _, _, err := receivePhaseSources(intent, "site", []string{"10.90.0.2/32"}); err == nil {
		t.Fatal("injected source exception admitted")
	}
}
