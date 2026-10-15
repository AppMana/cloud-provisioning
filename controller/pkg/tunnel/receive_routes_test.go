package tunnel

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"testing"
)

func routeTransitionFixture(t *testing.T) ReceiveTransitionPlan {
	t.Helper()
	old, site, root, other := receiverTestKey(8), receiverTestKey(16), receiverTestKey(24), receiverTestKey(32)
	before := PeerListDoc{Peers: []PeerSpec{{PublicKey: old, WGAllowedIPs: []string{"10.90.0.0/24", "2001:db8:1::/64"}}}}
	desired := PeerListDoc{Peers: []PeerSpec{{PublicKey: site, WGAllowedIPs: []string{"10.90.0.0/24", "2001:db8:1::/64"}, RouteHosts: []string{"10.90.0.1", "2001:db8:1::1"}}, {PublicKey: other, Remote: true, WGAllowedIPs: []string{"172.29.0.2/32"}, RouteHosts: []string{"172.29.0.2"}}}}
	base, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	p := ReceiveTransitionPlan{Version: 1, MeshUID: "mesh", CloudNodeUID: "cloud", RootPublicKey: root, Base: base, Desired: desired, Native: []netip.Prefix{netip.MustParsePrefix("172.29.0.0/24")}, Ports: map[string]int{}, Bindings: map[string]ReceiveBinding{}, Sites: map[string]ReceiveTransitionSite{}}
	p.SiteDesired = PeerListDoc{Peers: []PeerSpec{{PublicKey: root, Remote: true, Endpoint: "198.51.100.1:51820", WGAllowedIPs: []string{"172.29.0.1/32", "2001:db8:2::1/128"}, RouteHosts: []string{"172.29.0.1"}}, {PublicKey: other, Remote: true, Endpoint: "198.51.100.2:51820", WGAllowedIPs: []string{"172.29.0.2/32"}}}}
	prepared, err := PlanReceiveHandover(p.MeshUID, before, desired, p.Native)
	if err != nil {
		t.Fatal(err)
	}
	for i, path := range prepared.Receivers {
		p.Bindings[path.PublicKey] = ReceiveBinding{Interface: path.Interface, PeerPublicKey: path.PublicKey, PublicKey: receiverTestKey(byte(40 + 8*i)), ListenPort: 51821 + i}
		p.Ports[path.PublicKey] = 51821 + i
	}
	client, err := PlanSiteReceiveClient(p.MeshUID, site, root, p.Bindings[site], p.SiteDesired, p.SiteDesired)
	if err != nil {
		t.Fatal(err)
	}
	p.Sites[site] = ReceiveTransitionSite{NodeName: "site-node", NodeUID: "site", Client: client}
	return p
}

func TestReceiveRoutesFollowDesiredOwnerAndPreserveNativePaths(t *testing.T) {
	p := routeTransitionFixture(t)
	before, _ := json.Marshal(p)
	routes, err := PlanReceiveRoutes(p)
	if err != nil {
		t.Fatal(err)
	}
	site := receiverTestKey(16)
	if len(routes) != 2 || len(routes["cloud"]) != 4 || len(routes["site"]) != 2 {
		t.Fatalf("unexpected routes: %+v", routes)
	}
	for _, selection := range routes["cloud"] {
		if selection.PeerPublicKey != site || selection.PublicKey != p.Bindings[site].PublicKey || selection.Interface != p.Bindings[site].Interface {
			t.Fatal("cloud route uses the old or another peer")
		}
	}
	for _, selection := range routes["site"] {
		if selection.PublicKey != site || selection.PeerPublicKey != p.Bindings[site].PublicKey || selection.Interface != p.Sites[site].Client.Interface {
			t.Fatal("site route uses another cloud transport")
		}
	}
	if _, exists := routes["cloud"]["172.29.0.2/32"]; exists {
		t.Fatal("cloud-native route redirected")
	}
	if _, exists := routes["site"]["172.29.0.2/32"]; exists {
		t.Fatal("another cloud worker's route redirected")
	}
	after, _ := json.Marshal(p)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("route derivation changed source permissions")
	}
}

func TestReceiveRoutesRejectIncompleteOrExpandedPlans(t *testing.T) {
	for _, fault := range []string{"missing-site-document", "native-overlap", "route-outside-source", "broad-host", "site-route-outside-source", "missing-site", "duplicate-node", "extra-site", "client-permission", "client-endpoint", "binding-port", "binding-interface"} {
		t.Run(fault, func(t *testing.T) {
			p := routeTransitionFixture(t)
			key := receiverTestKey(16)
			site := p.Sites[key]
			switch fault {
			case "missing-site-document":
				p.SiteDesired.Peers = nil
			case "native-overlap":
				p.Native = append(p.Native, netip.MustParsePrefix("10.90.0.0/24"))
			case "route-outside-source":
				p.Desired.Peers[0].RouteHosts = []string{"172.29.0.3"}
			case "broad-host":
				p.Desired.Peers[0].RouteHosts = []string{"10.90.0.0/24"}
			case "site-route-outside-source":
				p.SiteDesired.Peers[0].RouteHosts = []string{"172.29.0.2"}
			case "missing-site":
				delete(p.Sites, key)
			case "duplicate-node":
				site.NodeUID = p.CloudNodeUID
				p.Sites[key] = site
			case "extra-site":
				p.Sites[receiverTestKey(8)] = site
			case "client-permission":
				site.Client.AllowedSources = []string{"0.0.0.0/0"}
				p.Sites[key] = site
			case "client-endpoint":
				site.Client.Endpoint = "198.51.100.2:51821"
				p.Sites[key] = site
			case "binding-port":
				p.Ports[key]++
			case "binding-interface":
				b := p.Bindings[key]
				b.Interface = "wg0"
				p.Bindings[key] = b
			}
			if routes, err := PlanReceiveRoutes(p); err == nil || routes != nil {
				t.Fatal("unsafe route plan accepted")
			}
		})
	}
}
