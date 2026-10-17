package tunnel

import (
	"encoding/json"
	"reflect"
	"testing"
)

func neighborFixture() PeerSpec {
	return PeerSpec{PublicKey: receiverTestKey(120), Remote: true, Endpoint: "198.51.100.9:51820", WGAllowedIPs: []string{"172.29.0.9/32", "2001:db8:2::9/128"}, RouteHosts: []string{"172.29.0.9", "2001:db8:2::9"}}
}

func TestReceiveNeighborsPreserveJournalOwnership(t *testing.T) {
	plans := routeSetFixture(t)
	peer := neighborFixture()
	peers := []PeerSpec{plans[0].SiteDesired.Peers[0], peer, plans[1].SiteDesired.Peers[0]}
	before, _ := json.Marshal(plans)
	got, err := PlanReceiveNeighborPeers(peers, plans)
	if err != nil || !reflect.DeepEqual(got, []PeerSpec{peer}) {
		t.Fatalf("unrelated peer was not separated: %+v %v", got, err)
	}
	got[0].WGAllowedIPs[0] = "changed"
	got[0].RouteHosts[0] = "changed"
	if peers[1].WGAllowedIPs[0] != "172.29.0.9/32" || peers[1].RouteHosts[0] != "172.29.0.9" {
		t.Fatal("planner returned writable aliases of publication")
	}
	after, _ := json.Marshal(plans)
	if string(before) != string(after) {
		t.Fatal("planner changed committed plans")
	}
	// Ordinary peer absence never means that a reserved worker can be removed.
	got, err = PlanReceiveNeighborPeers([]PeerSpec{}, plans)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatal("authoritative empty ordinary set was confused with absent publication", err)
	}
}

func TestReceiveNeighborsRejectConflictingAuthorization(t *testing.T) {
	for _, fault := range []string{"worker-v4", "worker-v6", "broader-worker", "second-worker", "site-source", "site-source-v6", "receiver-key", "site-key", "duplicate-key", "competing-prefix", "bad-key", "broad-route", "route-outside-source", "noncanonical", "default", "mapped", "transit", "missing-plan", "missing-document", "invalid-plan"} {
		t.Run(fault, func(t *testing.T) {
			plans := routeSetFixture(t)
			peer := neighborFixture()
			peer.RouteHosts = nil
			peers := []PeerSpec{peer}
			switch fault {
			case "worker-v4":
				peers[0].WGAllowedIPs = []string{"172.29.0.1/32"}
			case "worker-v6":
				peers[0].WGAllowedIPs = []string{"2001:db8:2::1/128"}
			case "broader-worker":
				peers[0].WGAllowedIPs = []string{"172.29.0.0/24"}
			case "second-worker":
				peers[0].WGAllowedIPs = []string{"172.29.0.3/32"}
			case "site-source":
				peers[0].WGAllowedIPs = []string{"10.90.0.7/32"}
			case "site-source-v6":
				peers[0].WGAllowedIPs = []string{"2001:db8:1::7/128"}
			case "receiver-key":
				peers[0].PublicKey = plans[0].Bindings[receiverTestKey(16)].PublicKey
			case "site-key":
				peers[0].PublicKey = receiverTestKey(16)
			case "duplicate-key":
				peers = append(peers, peer)
			case "competing-prefix":
				other := peer
				other.PublicKey = receiverTestKey(128)
				peers = append(peers, other)
			case "bad-key":
				peers[0].PublicKey = "invalid"
			case "broad-route":
				peers[0].RouteHosts = []string{"172.29.0.0/24"}
			case "route-outside-source":
				peers[0].RouteHosts = []string{"172.29.0.10"}
			case "noncanonical":
				peers[0].WGAllowedIPs = []string{"172.29.0.9/24"}
			case "default":
				peers[0].WGAllowedIPs = []string{"0.0.0.0/0"}
			case "mapped":
				peers[0].WGAllowedIPs = []string{"::ffff:172.29.0.9/128"}
			case "transit":
				peers[0].Transit = peers[0].WGAllowedIPs
			case "missing-plan":
				plans = nil
			case "missing-document":
				peers = nil
			case "invalid-plan":
				plans[0].Bindings = nil
			}
			if got, err := PlanReceiveNeighborPeers(peers, plans); err == nil || got != nil {
				t.Fatalf("accepted %s: %+v %v", fault, got, err)
			}
		})
	}
}
