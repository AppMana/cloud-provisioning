package tunnel

import "testing"

func TestReceiveLocalNetworkRejectsAllocationAndSourceDrift(t *testing.T) {
	base := ReceiveTransitionPlan{CloudNodeUID: "cloud", RootPublicKey: "root", CloudAddresses: []string{"10.88.0.3/24"}, SiteDesired: PeerListDoc{Peers: []PeerSpec{{PublicKey: "root", RouteHosts: []string{"10.88.0.3"}}}}, Sites: map[string]ReceiveTransitionSite{"public": {NodeUID: "site", RouteSources: []string{"10.10.0.1"}, Legacy: &ReceiveSitePath{Sources: []string{"10.10.0.1/32"}, Tunnel: &ReceiveSiteTunnel{Addresses: []string{"10.88.0.1/24"}}}, Forward: &ReceiveSitePath{Sources: []string{"10.10.0.1/32"}}}}}
	if err := validateReceiveLocalNetwork(base); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"unpublished", "duplicate-family", "default-prefix", "missing-cloud", "source-permission", "missing-site-sources"} {
		t.Run(fault, func(t *testing.T) {
			p := base
			site := base.Sites["public"]
			p.Sites = map[string]ReceiveTransitionSite{"public": site}
			switch fault {
			case "unpublished":
				p.CloudAddresses = []string{"10.88.0.4/24"}
			case "duplicate-family":
				p.CloudAddresses = []string{"10.88.0.3/24", "10.88.0.3/32"}
			case "default-prefix":
				p.CloudAddresses = []string{"10.88.0.3/0"}
			case "missing-cloud":
				p.CloudAddresses = nil
			case "source-permission":
				site.RouteSources = []string{"10.10.0.2"}
				p.Sites["public"] = site
			case "missing-site-sources":
				site.RouteSources = nil
				p.Sites["public"] = site
			}
			if err := validateReceiveLocalNetwork(p); err == nil {
				t.Fatal("unsafe local network projection accepted")
			}
		})
	}
	own, allocated, err := ReceiveLocalNetwork(base, "site")
	if err != nil {
		t.Fatal(err)
	}
	own[0], allocated[0] = "changed", "changed"
	if base.Sites["public"].RouteSources[0] != "10.10.0.1" || base.Sites["public"].Legacy.Tunnel.Addresses[0] != "10.88.0.1/24" {
		t.Fatal("native inputs mutated committed plan")
	}
}
