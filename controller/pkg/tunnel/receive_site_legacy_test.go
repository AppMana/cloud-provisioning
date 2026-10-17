package tunnel

import (
	"encoding/json"
	"reflect"
	"testing"
)

func legacySiteFixture() (map[string][]byte, PeerListDoc, string, string) {
	old, site, cloud := receiverTestKey(8), receiverTestKey(16), receiverTestKey(24)
	data := map[string][]byte{
		NodePublicKeyPrefix + "old": []byte(old), NodeTunnelAddressPrefix + "old": []byte("10.88.0.1/24"), SiteAddressesPrefix + "old": []byte("10.10.0.1,2001:db8:1::1"),
		NodePublicKeyPrefix + "site": []byte(site), NodeAddressesPrefix + "site": []byte("10.10.0.2,2001:db8:1::2"), NodePodCIDRsPrefix + "site": []byte("10.90.0.0/24,2001:db8:90::/64"), NodeTunnelAddressPrefix + "site": []byte("10.88.0.2/24"),
		RelayPublicKeyKey:              []byte(site),
		PeerPublicKeyPrefix + "worker": []byte(cloud), PeerEndpointPrefix + "worker": []byte("198.51.100.1:51820"), PeerAllowedIPsPrefix + "worker": []byte("172.29.0.1/32,2001:db8:2::1/128,172.30.0.0/24"), PeerRouteHostsPrefix + "worker": []byte("172.29.0.1,2001:db8:2::1"),
		PeerPublicKeyPrefix + "other": []byte(receiverTestKey(32)), PeerEndpointPrefix + "other": []byte("198.51.100.2:51820"), PeerAllowedIPsPrefix + "other": []byte("172.29.0.2/32"), PeerRouteHostsPrefix + "other": []byte("172.29.0.2"),
	}
	base := PeerListDoc{Peers: []PeerSpec{{PublicKey: old, WGAllowedIPs: []string{"10.88.0.1/32", "10.10.0.1/32", "2001:db8:1::1/128", "10.10.0.2/32", "2001:db8:1::2/128", "10.90.0.0/24", "2001:db8:90::/64"}, RouteHosts: []string{"10.10.0.1", "2001:db8:1::1", "10.10.0.2", "2001:db8:1::2"}, TransitHosts: []string{"10.10.0.2", "2001:db8:1::2"}}}}
	return data, base, site, cloud
}

func TestReceiveSiteLegacyKeepsAppliedRelayAcrossElection(t *testing.T) {
	data, base, site, cloud := legacySiteFixture()
	before, _ := json.Marshal(data)
	elected, err := SiteTransit(data, nil)
	if err != nil || elected == nil || elected.Via != "10.10.0.2" {
		t.Fatalf("election: %+v %v", elected, err)
	}
	path, err := PlanReceiveSiteLegacy(data, "site", site, cloud, base)
	if err != nil {
		t.Fatal(err)
	}
	if path.PeerPublicKey != receiverTestKey(8) || path.Transit == nil || path.Transit.Via != "10.10.0.1" || path.Transit.ViaOtherFamily != "2001:db8:1::1" {
		t.Fatalf("lost applied relay: %+v", path)
	}
	if !reflect.DeepEqual(path.Transit.Hosts, []string{"172.29.0.1", "2001:db8:2::1"}) || !reflect.DeepEqual(path.Transit.Blocks, []string{"172.30.0.0/24"}) {
		t.Fatalf("worker scope: %+v", path.Transit)
	}
	direct, err := PlanReceiveSiteLegacy(data, "old", receiverTestKey(8), cloud, base)
	if err != nil || direct.Transit != nil || direct.PeerPublicKey != receiverTestKey(8) {
		t.Fatalf("direct path: %+v %v", direct, err)
	}
	after, _ := json.Marshal(data)
	if string(before) != string(after) {
		t.Fatal("changed current election")
	}
}

func TestReceiveSiteLegacyRejectsUnboundSourcesAndNextHops(t *testing.T) {
	for _, fault := range []string{"wrong-key", "missing-source", "unaccepted-source", "split-owner", "partial-pod-block", "missing-relay", "duplicate-relay", "missing-gateway", "unaccepted-gateway", "relayed-gateway", "missing-family", "missing-worker"} {
		t.Run(fault, func(t *testing.T) {
			data, base, site, cloud := legacySiteFixture()
			switch fault {
			case "wrong-key":
				site = receiverTestKey(40)
			case "missing-source":
				delete(data, NodeAddressesPrefix+"site")
				delete(data, NodePodCIDRsPrefix+"site")
			case "unaccepted-source":
				data[NodeAddressesPrefix+"site"] = []byte("10.10.0.3")
			case "split-owner":
				base.Peers[0].WGAllowedIPs = base.Peers[0].WGAllowedIPs[:5]
				base.Peers = append(base.Peers, PeerSpec{PublicKey: site, WGAllowedIPs: []string{"10.90.0.0/24", "2001:db8:90::/64"}})
			case "partial-pod-block":
				base.Peers[0].WGAllowedIPs[5] = "10.90.0.0/25"
			case "missing-relay":
				delete(data, NodePublicKeyPrefix+"old")
			case "duplicate-relay":
				data[NodePublicKeyPrefix+"duplicate"] = []byte(receiverTestKey(8))
			case "missing-gateway":
				delete(data, SiteAddressesPrefix+"old")
			case "unaccepted-gateway":
				base.Peers[0].WGAllowedIPs[1] = "10.10.0.9/32"
			case "relayed-gateway":
				data[SiteAddressesPrefix+"old"] = []byte("10.10.0.2,2001:db8:1::2")
			case "missing-family":
				data[SiteAddressesPrefix+"old"] = []byte("10.10.0.1")
			case "missing-worker":
				delete(data, PeerPublicKeyPrefix+"worker")
			}
			if _, err := PlanReceiveSiteLegacy(data, "site", site, cloud, base); err == nil {
				t.Fatal("accepted unbound legacy path")
			}
		})
	}
}

func TestReceiveRoutesValidateCommittedLegacyTransit(t *testing.T) {
	for _, fault := range []string{"none", "owner", "gateway", "destination", "missing-transit", "source"} {
		t.Run(fault, func(t *testing.T) {
			p := routeTransitionFixture(t)
			old, site := receiverTestKey(8), receiverTestKey(16)
			var base PeerListDoc
			if err := json.Unmarshal(p.Base, &base); err != nil {
				t.Fatal(err)
			}
			base.Peers[0].WGAllowedIPs = append(base.Peers[0].WGAllowedIPs, "10.10.0.1/32", "2001:db8::1/128")
			base.Peers[0].RouteHosts = []string{"10.10.0.1", "2001:db8::1"}
			p.Base, _ = json.Marshal(base)
			legacy := &ReceiveSiteLegacy{Sources: []string{"10.90.0.0/24", "2001:db8:1::/64"}, PeerPublicKey: old, Transit: &TransitSpec{Via: "10.10.0.1", ViaOtherFamily: "2001:db8::1"}}
			setTransitDestinations(legacy.Transit, []PeerSpec{p.SiteDesired.Peers[0]})
			member := p.Sites[site]
			member.Legacy = legacy
			p.Sites[site] = member
			switch fault {
			case "owner":
				legacy.PeerPublicKey = site
			case "gateway":
				legacy.Transit.Via = "10.10.0.9"
			case "destination":
				legacy.Transit.Blocks = append(legacy.Transit.Blocks, "192.0.2.0/24")
			case "missing-transit":
				legacy.Transit = nil
			case "source":
				legacy.Sources = append(legacy.Sources, "10.91.0.0/24")
			}
			_, err := PlanReceiveRoutes(p)
			if (err != nil) != (fault != "none") {
				t.Fatalf("legacy admission: %v", err)
			}
		})
	}
}
