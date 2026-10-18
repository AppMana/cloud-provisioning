package tunnel

import (
	"encoding/json"
	"slices"
	"testing"
)

// Planner coverage only: old source ownership must survive a desired change.
// These cases do not establish native migration or permission retirement.
func TestReceiveSiteBasePreservesPreviousDestinations(t *testing.T) {
	for _, prefix := range []string{"10.244.159.0/26", "2001:db8:159::/64"} {
		for _, withdrawal := range []bool{false, true} {
			p := routeSetFixture(t)[0]
			raw, _ := json.Marshal(p.SiteDesired)
			var old PeerListDoc
			if err := json.Unmarshal(raw, &old); err != nil {
				t.Fatal(err)
			}
			p.SiteBase = &old
			if withdrawal {
				old.Peers[0].WGAllowedIPs = append(old.Peers[0].WGAllowedIPs, prefix)
			} else {
				p.SiteDesired.Peers[0].WGAllowedIPs = append(p.SiteDesired.Peers[0].WGAllowedIPs, prefix)
			}
			for key, site := range p.Sites {
				var err error
				site.Client, err = PlanSiteReceiveClient(p.MeshUID, key, p.RootPublicKey, p.Bindings[key], p.PreviousSitePeers(), p.SiteDesired)
				if err != nil {
					t.Fatal(err)
				}
				p.Sites[key] = site
				if !slices.Contains(site.Client.AllowedSources, prefix) {
					t.Fatal("lost transitional receive permission")
				}
			}
			routes, err := PlanReceiveRoutes(p)
			if err != nil {
				t.Fatal(err)
			}
			_, selected := routes["site"][prefix]
			if selected == withdrawal {
				t.Fatal("selected old destination as desired or omitted added destination")
			}
			neighbor := neighborFixture()
			neighbor.WGAllowedIPs, neighbor.RouteHosts = []string{prefix}, nil
			if _, err := PlanReceiveNeighborPeers([]PeerSpec{neighbor}, []ReceiveTransitionPlan{p}); err == nil {
				t.Fatal("ordinary neighbor acquired a transition-owned destination")
			}
			other := routeSetFixture(t)[1]
			other.SiteDesired.Peers[0].WGAllowedIPs = append(other.SiteDesired.Peers[0].WGAllowedIPs, prefix)
			refreshRouteSetClients(t, &other)
			for _, plans := range [][]ReceiveTransitionPlan{{p, other}, {other, p}} {
				if _, err := PlanReceiveRouteSet(plans); err == nil {
					t.Fatal("another journal acquired a transition-owned destination")
				}
			}
		}
	}
}

func TestReceiveSiteBaseRejectsUnownedRoute(t *testing.T) {
	p := routeTransitionFixture(t)
	old := p.SiteDesired
	old.Peers = slices.Clone(old.Peers)
	old.Peers[0].RouteHosts = []string{"10.244.159.1"}
	p.SiteBase = &old
	if _, err := PlanReceiveRoutes(p); err == nil {
		t.Fatal("accepted old route outside admitted source permissions")
	}
	old.Peers = old.Peers[1:]
	if _, err := PlanReceiveRoutes(p); err == nil {
		t.Fatal("accepted previous document without worker identity")
	}
}
