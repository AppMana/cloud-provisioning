package tunnel

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestReceiveSiteForwardTracksDesiredOwnerAcrossRelayChange(t *testing.T) {
	data, base, site, cloud := legacySiteFixture()
	old := receiverTestKey(8)
	desired := PeerListDoc{Peers: []PeerSpec{
		{PublicKey: site, WGAllowedIPs: append([]string{"10.88.0.2/32"}, base.Peers[0].WGAllowedIPs[1:]...), RouteHosts: []string{"10.10.0.2", "2001:db8:1::2", "10.10.0.1", "2001:db8:1::1"}, TransitHosts: []string{"10.10.0.1", "2001:db8:1::1"}},
		{PublicKey: old, WGAllowedIPs: []string{"10.88.0.1/32"}},
	}}
	peers, err := SitePeers(data)
	if err != nil {
		t.Fatal(err)
	}
	var worker PeerSpec
	for _, p := range peers {
		if p.PublicKey == cloud {
			worker = p
		}
	}
	before, _ := json.Marshal(data)
	for _, tc := range []struct {
		node, public       string
		oldRelay, newRelay bool
	}{{"site", site, true, false}, {"old", old, false, true}} {
		t.Run(tc.node, func(t *testing.T) {
			legacy, err := PlanReceiveSiteLegacy(data, tc.node, tc.public, cloud, base)
			if err != nil {
				t.Fatal(err)
			}
			forward, err := PlanReceiveSitePath(data, tc.node, tc.public, cloud, desired)
			if err != nil {
				t.Fatal(err)
			}
			member := ReceiveTransitionSite{Legacy: &legacy, Forward: &forward}
			if err := ValidateReceiveSiteForward(member, tc.public, desired, worker); err != nil {
				t.Fatal(err)
			}
			if (legacy.Transit != nil) != tc.oldRelay || (forward.Transit != nil) != tc.newRelay || legacy.PeerPublicKey != old || forward.PeerPublicKey != site {
				t.Fatal("forwarding generation lost source ownership")
			}
			if forward.Transit != nil && (forward.Transit.Via != "10.10.0.2" || forward.Transit.ViaOtherFamily != "2001:db8:1::2") {
				t.Fatal("desired relay gateways differ")
			}
			// The mutable election cannot rewrite either persisted generation.
			data[RelayPublicKeyKey] = []byte(old)
			again, err := PlanReceiveSitePath(data, tc.node, tc.public, cloud, desired)
			data[RelayPublicKeyKey] = []byte(site)
			if err != nil || !reflect.DeepEqual(forward, again) {
				t.Fatal("forwarding followed later election", err)
			}
			for _, fault := range []string{"owner", "sources", "allocation", "missing-allocation", "missing-legacy", "exception", "relay"} {
				t.Run(fault, func(t *testing.T) {
					raw, _ := json.Marshal(member)
					var broken ReceiveTransitionSite
					if err := json.Unmarshal(raw, &broken); err != nil {
						t.Fatal(err)
					}
					switch fault {
					case "owner":
						broken.Forward.PeerPublicKey = old
					case "sources":
						broken.Forward.Sources = []string{"10.10.0.99/32"}
					case "allocation":
						broken.Forward.Tunnel.Addresses = []string{"10.88.0.99/24"}
					case "missing-allocation":
						broken.Forward.Tunnel = nil
					case "missing-legacy":
						broken.Legacy = nil
					case "exception":
						broken.Forward.Tunnel.Hosts = []string{"172.29.0.1/32"}
					case "relay":
						if broken.Forward.Transit != nil {
							broken.Forward.Transit.Via = "10.10.0.99"
						} else {
							broken.Forward.Transit = legacy.Transit
						}
					}
					if err := ValidateReceiveSiteForward(broken, tc.public, desired, worker); err == nil {
						t.Fatal("unbound forward path admitted")
					}
				})
			}
		})
	}
	after, _ := json.Marshal(data)
	if string(before) != string(after) {
		t.Fatal("planner changed mesh election")
	}
}

func TestReceiveSourceRoutesSelectForwardRelayExceptions(t *testing.T) {
	p := sourceRoutePlans(t)[0]
	old, site := receiverTestKey(8), receiverTestKey(16)
	for i := range p.Desired.Peers {
		peer := &p.Desired.Peers[i]
		if peer.PublicKey == site {
			peer.WGAllowedIPs = append(peer.WGAllowedIPs, "10.88.0.8/32", "fd20::8/128", "10.10.0.2/32", "2001:db8::2/128")
			peer.RouteHosts = append(peer.RouteHosts, "10.10.0.2", "2001:db8::2")
		}
	}
	p.Desired.Peers = append(p.Desired.Peers, PeerSpec{PublicKey: old, WGAllowedIPs: []string{"10.88.0.9/32", "fd20::9/128"}})
	for public, member := range p.Sites {
		forward := &ReceiveSitePath{Sources: append([]string(nil), member.Legacy.Sources...), PeerPublicKey: site}
		if public == old {
			forward.Transit = &TransitSpec{Via: "10.10.0.2", ViaOtherFamily: "2001:db8::2"}
			setTransitDestinations(forward.Transit, []PeerSpec{p.SiteDesired.Peers[0]})
		}
		allocation, err := planReceiveSiteTunnel(member.Legacy.Tunnel.Addresses, public, p.Desired, p.SiteDesired.Peers[0], forward.Transit != nil)
		if err != nil {
			t.Fatal(err)
		}
		forward.Tunnel = &allocation
		member.Forward = forward
		p.Sites[public] = member
	}
	routes, err := PlanReceiveRouteSet([]ReceiveTransitionPlan{p})
	if err != nil {
		t.Fatal(err)
	}
	intent := ReceiveJournalRoutes{Plans: map[string]ReceiveTransitionPlan{p.MachineUID: p}, Receivers: routes, Legacy: map[string]ReceiveTransitionPlan{}}
	selected, err := PlanReceiveSourceRoutes(intent)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected["site"].Hosts) != 0 || !reflect.DeepEqual(selected["retained"].Hosts, []string{"10.88.0.128/32"}) {
		t.Fatalf("desired relay exceptions: %+v", selected)
	}
	intent.Legacy[p.MachineUID] = p
	for node := range intent.Receivers {
		intent.Receivers[node] = map[string]ReceiveRouteSelection{}
	}
	selected, err = PlanReceiveSourceRoutes(intent)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected["retained"].Hosts) != 0 || !reflect.DeepEqual(selected["site"].Hosts, []string{"10.88.0.128/32"}) {
		t.Fatalf("rollback relay exceptions: %+v", selected)
	}
	member := p.Sites[old]
	member.Forward = nil
	p.Sites[old] = member
	if _, err := PlanReceiveRoutes(p); err == nil {
		t.Fatal("partial desired forwarding projection admitted")
	}
}
