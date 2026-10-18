package tunnel

import (
	"encoding/json"
	"reflect"
	"testing"
)

func neighborFixture() PeerSpec {
	return PeerSpec{PublicKey: receiverTestKey(120), Remote: true, Endpoint: "198.51.100.9:51820", WGAllowedIPs: []string{"172.29.0.9/32", "2001:db8:2::9/128"}, RouteHosts: []string{"172.29.0.9", "2001:db8:2::9"}}
}

func TestReceiveCloudNeighborsKeepIndependentJournalEdges(t *testing.T) {
	plans := routeSetFixture(t)
	root := plans[0].RootPublicKey
	other := plans[1].SiteDesired.Peers[0]
	other.Remote = true
	peers := []PeerSpec{plans[0].SiteDesired.Peers[0], other, neighborFixture()}
	got, err := PlanReceiveCloudNeighborPeers(peers, plans, root)
	if err != nil || !reflect.DeepEqual(got, peers[1:]) {
		t.Fatalf("lost direct cloud edge for journal-owned worker: %+v %v", got, err)
	}
	retiring, err := PlanReceiveCloudNeighborRetirements(got, []PeerSpec{neighborFixture()}, plans, root)
	if err != nil || !reflect.DeepEqual(retiring, []PeerSpec{other}) {
		t.Fatalf("cloud edge withdrawal changed site-facing journal ownership: %+v %v", retiring, err)
	}
	// A site must continue excluding both reserved workers from ordinary work.
	site, err := PlanReceiveNeighborPeers(peers, plans)
	if err != nil || !reflect.DeepEqual(site, []PeerSpec{neighborFixture()}) {
		t.Fatal("cloud projection weakened site journal ownership", err)
	}
	for _, fault := range []string{"other-owner", "other-receiver", "site-source", "own-source", "unknown-root", "empty-root"} {
		t.Run(fault, func(t *testing.T) {
			peer := other
			peer.RouteHosts = nil
			local := root
			switch fault {
			case "other-owner":
				peer.PublicKey = receiverTestKey(121)
			case "other-receiver":
				peer.PublicKey = plans[1].Bindings[receiverTestKey(8)].PublicKey
			case "site-source":
				peer.WGAllowedIPs = []string{"10.90.0.7/32"}
			case "own-source":
				peer.WGAllowedIPs = []string{"172.29.0.1/32"}
			case "unknown-root":
				local = receiverTestKey(122)
			case "empty-root":
				local = ""
			}
			if _, err := PlanReceiveCloudNeighborPeers([]PeerSpec{peer}, plans, local); err == nil {
				t.Fatal("conflicting cloud edge admitted")
			}
			if _, err := PlanReceiveCloudNeighborRetirements([]PeerSpec{peer}, []PeerSpec{}, plans, local); err == nil {
				t.Fatal("conflicting cloud edge authorized retirement")
			}
		})
	}
}

func TestReceiveNeighborRetirementIntent(t *testing.T) {
	plans := routeSetFixture(t)
	old := neighborFixture()
	replacement := old
	replacement.PublicKey = receiverTestKey(121)
	previous := []PeerSpec{plans[0].SiteDesired.Peers[0], old, plans[1].SiteDesired.Peers[0]}
	// Address reuse must retain the old owner's identity for explicit retirement;
	// neither reserved worker belongs in that retirement set.
	got, err := PlanReceiveNeighborRetirements(previous, []PeerSpec{replacement}, plans)
	if err != nil || !reflect.DeepEqual(got, []PeerSpec{old}) {
		t.Fatalf("lost retiring owner or included journal owner: %+v %v", got, err)
	}
	got[0].WGAllowedIPs[0] = "changed"
	if old.WGAllowedIPs[0] != "172.29.0.9/32" {
		t.Fatal("retirement intent aliases previous publication")
	}
	got, err = PlanReceiveNeighborRetirements(previous, []PeerSpec{old}, plans)
	if err != nil || len(got) != 0 {
		t.Fatal("still-published owner was retired", err)
	}
	got, err = PlanReceiveNeighborRetirements(previous, []PeerSpec{}, plans)
	if err != nil || len(got) != 1 {
		t.Fatal("last ordinary owner was not retained for retirement", err)
	}
	if _, err := PlanReceiveNeighborRetirements(previous, nil, plans); err == nil {
		t.Fatal("missing publication authorized retirement")
	}
	old.WGAllowedIPs = []string{"172.29.0.1/32"}
	old.RouteHosts = nil
	if _, err := PlanReceiveNeighborRetirements([]PeerSpec{old}, []PeerSpec{}, plans); err == nil {
		t.Fatal("retirement overlapped a reserved worker")
	}
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

func TestReceiveNeighborRetirementPreservesExactGrowthVariants(t *testing.T) {
	plans := routeSetFixture(t)
	old := neighborFixture()
	grown := old
	grown.WGAllowedIPs = append(append([]string{}, old.WGAllowedIPs...), "172.29.99.1/32")
	got, err := PlanReceiveNeighborRetirements([]PeerSpec{old, grown}, []PeerSpec{}, plans)
	if err != nil || !reflect.DeepEqual(got, []PeerSpec{old, grown}) {
		t.Fatalf("lost exact variants: %+v %v", got, err)
	}
	foreign := neighborFixture()
	foreign.PublicKey = receiverTestKey(121)
	foreign.WGAllowedIPs = []string{"172.29.99.1/32"}
	foreign.RouteHosts = nil
	historical := []PeerSpec{old, grown, foreign}
	got, err = PlanReceiveNeighborRetirements(historical, []PeerSpec{}, plans)
	if err != nil || !reflect.DeepEqual(got, historical) {
		t.Fatalf("lost overlapping historical attempts: %+v %v", got, err)
	}
	if _, err := PlanReceiveNeighborPeers([]PeerSpec{grown, foreign}, plans); err == nil {
		t.Fatal("overlapping desired owners accepted")
	}
}

// Pure planning tests: persisted withdrawal, native ownership, receipt identity
// and traffic continuity are separate requirements. Historical variants are
// deliberately independent; actual live permissions must match before deletion.
func TestReceiveWithdrawnNeighborPeers(t *testing.T) {
	for _, fault := range []string{"none", "republished", "missing-document", "missing-plan", "unknown-root", "site-source", "other-worker", "transit", "current-owner", "outside-route", "receiver-identity", "empty-root", "default-source", "site-role"} {
		t.Run(fault, func(t *testing.T) {
			plans := routeSetFixture(t)
			root := plans[0].RootPublicKey
			old := plans[0].SiteDesired.Peers[0]
			old.Remote = true
			historical := old
			historical.WGAllowedIPs = []string{"172.29.0.8/32"}
			historical.RouteHosts = []string{"172.29.0.8"}
			current := []PeerSpec{neighborFixture()}
			switch fault {
			case "republished":
				current = append(current, old)
			case "missing-document":
				current = nil
			case "missing-plan":
				plans = nil
			case "unknown-root":
				root = receiverTestKey(123)
			case "receiver-identity":
				root = plans[0].Bindings[receiverTestKey(8)].PublicKey
			case "empty-root":
				root = ""
			case "default-source":
				historical.WGAllowedIPs = []string{"0.0.0.0/0"}
				historical.RouteHosts = nil
			case "site-role":
				historical.Remote = false
			case "site-source":
				historical.WGAllowedIPs = []string{"10.90.0.7/32"}
				historical.RouteHosts = nil
			case "other-worker":
				historical.WGAllowedIPs = []string{"172.29.0.3/32"}
				historical.RouteHosts = nil
			case "transit":
				historical.Transit = []string{"172.29.0.8/32"}
			case "current-owner":
				historical.WGAllowedIPs = []string{"172.29.0.9/32"}
				historical.RouteHosts = nil
			case "outside-route":
				historical.RouteHosts = []string{"172.29.0.10"}
			}
			previous := []PeerSpec{old, historical, neighborFixture()}
			got, err := PlanReceiveWithdrawnNeighborPeers(previous, current, plans, root)
			if fault != "none" {
				if err == nil {
					t.Fatal("unsafe withdrawal admitted")
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, previous[:2]) {
				t.Fatalf("lost target variants or selected another owner: %+v %v", got, err)
			}
			got[0].WGAllowedIPs[0] = "changed"
			if previous[0].WGAllowedIPs[0] == "changed" {
				t.Fatal("aliased recorded permissions")
			}
			ordinary, err := PlanReceiveNeighborRetirements(previous, current, plans)
			if err != nil || len(ordinary) != 0 {
				t.Fatal("ordinary reconciliation lost reserved-root fence", err)
			}
		})
	}
}
