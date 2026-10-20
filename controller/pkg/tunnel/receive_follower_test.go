package tunnel

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// Pure planners with synthetic histories: these cases establish source-owner
// selection, not live admission, relay readiness, kernel routes or continuity.
func TestReceiveFollowerPathUsesSourceOwner(t *testing.T) {
	data, base, _, cloud := legacySiteFixture()
	delete(data, NodePublicKeyPrefix+"site")
	delete(data, NodeTunnelAddressPrefix+"site")
	peers, err := SitePeers(data)
	if err != nil {
		t.Fatal(err)
	}
	var worker PeerSpec
	for _, peer := range peers {
		if peer.PublicKey == cloud {
			worker = peer
		}
	}
	for _, fault := range []string{"none", "endpoint-key", "endpoint-v4", "endpoint-v6", "missing-source", "unaccepted-source", "partial-source", "split-owner", "missing-relay", "duplicate-relay", "relayed-gateway", "missing-family"} {
		t.Run(fault, func(t *testing.T) {
			raw, _ := json.Marshal(data)
			var current map[string][]byte
			if err := json.Unmarshal(raw, &current); err != nil {
				t.Fatal(err)
			}
			raw, _ = json.Marshal(base)
			var doc PeerListDoc
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "endpoint-key":
				current[NodePublicKeyPrefix+"site"] = []byte(receiverTestKey(16))
			case "endpoint-v4":
				current[NodeTunnelAddressPrefix+"site"] = []byte("10.88.0.2/24")
			case "endpoint-v6":
				current[NodeTunnelAddress6Prefix+"site"] = []byte("fd20::2/64")
			case "missing-source":
				delete(current, NodeAddressesPrefix+"site")
				delete(current, NodePodCIDRsPrefix+"site")
			case "unaccepted-source":
				current[NodeAddressesPrefix+"site"] = []byte("10.10.0.99")
			case "partial-source":
				doc.Peers[0].WGAllowedIPs[5] = "10.90.0.0/25"
			case "split-owner":
				doc.Peers[0].WGAllowedIPs = doc.Peers[0].WGAllowedIPs[:5]
				doc.Peers = append(doc.Peers, PeerSpec{PublicKey: receiverTestKey(16), WGAllowedIPs: []string{"10.90.0.0/24", "2001:db8:90::/64"}})
			case "missing-relay":
				delete(current, NodePublicKeyPrefix+"old")
			case "duplicate-relay":
				current[NodePublicKeyPrefix+"duplicate"] = current[NodePublicKeyPrefix+"old"]
			case "relayed-gateway":
				current[SiteAddressesPrefix+"old"] = []byte("10.10.0.2,2001:db8:1::2")
			case "missing-family":
				current[SiteAddressesPrefix+"old"] = []byte("10.10.0.1")
			}
			before, _ := json.Marshal(current)
			path, err := PlanReceiveFollowerPath(current, "site", doc, worker)
			if (err != nil) != (fault != "none") {
				t.Fatalf("follower admission: %+v, %v", path, err)
			}
			if err == nil {
				if path.Tunnel != nil || path.PeerPublicKey != receiverTestKey(8) || path.Transit == nil || path.Transit.Via != "10.10.0.1" || path.Transit.ViaOtherFamily != "2001:db8:1::1" {
					t.Fatalf("follower lost source owner or acquired a tunnel: %+v", path)
				}
				if !reflect.DeepEqual(path.Transit.Hosts, []string{"172.29.0.1", "2001:db8:2::1"}) || !reflect.DeepEqual(path.Transit.Blocks, []string{"172.30.0.0/24"}) {
					t.Fatal("follower included another worker's destinations")
				}
			}
			after, _ := json.Marshal(current)
			if string(before) != string(after) {
				t.Fatal("planner changed mesh ownership")
			}
		})
	}
}

func TestReceiveJournalFollowerTracksForwardAbortAndRollback(t *testing.T) {
	p := sourceRoutePlans(t)[0]
	old, next := receiverTestKey(8), receiverTestKey(16)
	// Both generations admit the follower pod block. Each owner's gateway
	// remains an owned host, distinct from the follower's relayed sources.
	for i := range p.Desired.Peers {
		if p.Desired.Peers[i].PublicKey == next {
			p.Desired.Peers[i].WGAllowedIPs = append(p.Desired.Peers[i].WGAllowedIPs, "10.10.0.2/32", "2001:db8::2/128")
			p.Desired.Peers[i].RouteHosts = append(p.Desired.Peers[i].RouteHosts, "10.10.0.2", "2001:db8::2")
		}
	}
	p, initial := journalRouteFixture(t, p)
	data := map[string][]byte{
		NodePublicKeyPrefix + "old": []byte(old), NodeAddressesPrefix + "old": []byte("10.10.0.1,2001:db8::1"),
		NodePublicKeyPrefix + "next": []byte(next), NodeAddressesPrefix + "next": []byte("10.10.0.2,2001:db8::2"),
		NodePodCIDRsPrefix + "follower": []byte("10.90.0.0/24,2001:db8:1::/64"),
		RelayPublicKeyKey:               []byte(next),
	}
	for _, direction := range []string{"forward", "abort", "rollback"} {
		t.Run(direction, func(t *testing.T) {
			tr := initial
			reversed := false
			for step := 0; step < 12; step++ {
				view, err := tr.View()
				if err != nil {
					t.Fatal(err)
				}
				snapshot := journalRouteSnapshot(t, p, tr)
				path, err := PlanReceiveJournalFollowerPath(data, "follower", snapshot, "cluster")
				if err != nil {
					t.Fatal(err)
				}
				want, gateway := next, "10.10.0.2"
				if step == 0 || direction == "abort" || (direction == "rollback" && reversed) {
					want, gateway = old, "10.10.0.1"
				}
				if path.PeerPublicKey != want || path.Transit == nil || path.Transit.Via != gateway || path.Tunnel != nil {
					t.Fatalf("wrong committed source owner: %+v", path)
				}
				if view.Complete {
					return
				}
				at := view.Round.IssuedAt.Add(time.Second)
				if at.Before(view.DrainUntil) {
					at = view.DrainUntil
				}
				if !reversed && ((direction == "abort" && step == 0) || (direction == "rollback" && step == 1)) {
					tr, err = tr.Rollback(at)
					if err != nil {
						t.Fatal(err)
					}
					reversed = true
				} else {
					tr = advanceJournalRoute(t, tr, at)
				}
			}
			t.Fatal("fixture did not complete")
		})
	}
}
