package tunnel

import (
	"encoding/json"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"reflect"
	"testing"
	"time"
)

func TestReceiveSiteTunnelPreservesOnlyAcceptedHostExceptions(t *testing.T) {
	site := receiverTestKey(16)
	addresses := []string{"fd20::2/64", "10.100.0.2/24"}
	base := PeerListDoc{Peers: []PeerSpec{{PublicKey: site, WGAllowedIPs: []string{"10.100.0.2/32", "fd20::2/128"}}}}
	cloud := PeerSpec{WGAllowedIPs: []string{"10.100.0.128/32", "fd20::128/128", "172.29.0.0/24"}, RouteHosts: []string{"10.100.0.128", "fd20::128", "172.29.0.1"}}
	for _, tc := range []struct {
		name        string
		relayed     bool
		permissions []string
		hosts       []string
	}{
		{"dual", true, base.Peers[0].WGAllowedIPs, []string{"10.100.0.128/32", "fd20::128/128"}},
		{"single accepted family", true, []string{"10.100.0.2/32"}, []string{"10.100.0.128/32"}},
		{"no old direct permission", true, nil, []string{}},
		{"direct workload path", false, base.Peers[0].WGAllowedIPs, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := PeerListDoc{Peers: []PeerSpec{{PublicKey: site, WGAllowedIPs: tc.permissions}}}
			got, err := planReceiveSiteTunnel(addresses, site, previous, cloud, tc.relayed)
			if err != nil || !reflect.DeepEqual(got.Hosts, tc.hosts) || !reflect.DeepEqual(got.Addresses, []string{"10.100.0.2/24", "fd20::2/64"}) {
				t.Fatalf("projection: %+v %v", got, err)
			}
		})
	}
	if _, err := planReceiveSiteTunnel(addresses, site, PeerListDoc{Peers: []PeerSpec{}}, cloud, false); err == nil {
		t.Fatal("direct path accepted unbound allocation")
	}
	for _, bad := range [][]string{nil, {"bad"}, {"10.100.0.2/0"}, {"10.100.0.2/24", "10.100.0.3/24"}, {"::ffff:10.100.0.2/128"}} {
		if _, err := planReceiveSiteTunnel(bad, site, base, cloud, true); err == nil {
			t.Fatal("invalid allocation accepted")
		}
	}
}

func sourceRoutePlans(t *testing.T) []ReceiveTransitionPlan {
	t.Helper()
	plans := routeSetFixture(t)
	old, site := receiverTestKey(8), receiverTestKey(16)
	for i := range plans {
		p := &plans[i]
		host := "10.88.0.128"
		v6 := "fd20::128"
		if i == 1 {
			host = "10.88.0.129"
			v6 = "fd20::129"
		}
		p.SiteDesired.Peers[0].WGAllowedIPs = []string{host + "/32", v6 + "/128"}
		p.SiteDesired.Peers[0].RouteHosts = []string{host}
		refreshRouteSetClients(t, p)
		var base PeerListDoc
		if err := json.Unmarshal(p.Base, &base); err != nil {
			t.Fatal(err)
		}
		base.Peers[0].WGAllowedIPs = append(base.Peers[0].WGAllowedIPs, "10.10.0.1/32", "2001:db8::1/128", "10.88.0.9/32", "fd20::9/128")
		base.Peers[0].RouteHosts = []string{"10.10.0.1", "2001:db8::1"}
		base.Peers = append(base.Peers, PeerSpec{PublicKey: site, WGAllowedIPs: []string{"10.88.0.8/32", "fd20::8/128"}})
		p.Base, _ = json.Marshal(base)
		for key, member := range p.Sites {
			addresses := []string{"10.88.0.8/24", "fd20::8/64"}
			sources := []string{"10.90.0.0/24", "2001:db8:1::/64"}
			legacy := &ReceiveSiteLegacy{Sources: sources, PeerPublicKey: old}
			if key == site {
				legacy.Transit = &TransitSpec{Via: "10.10.0.1", ViaOtherFamily: "2001:db8::1"}
				setTransitDestinations(legacy.Transit, []PeerSpec{p.SiteDesired.Peers[0]})
			} else {
				addresses = []string{"10.88.0.9/24", "fd20::9/64"}
			}
			exception, err := planReceiveSiteTunnel(addresses, key, base, p.SiteDesired.Peers[0], legacy.Transit != nil)
			if err != nil {
				t.Fatal(err)
			}
			legacy.Tunnel = &exception
			member.Legacy = legacy
			p.Sites[key] = member
		}
	}
	return plans
}

func TestReceiveSourceRoutesCombineMixedGenerations(t *testing.T) {
	plans := sourceRoutePlans(t)
	before, _ := json.Marshal(plans)
	all, err := PlanReceiveRouteSet(plans)
	if err != nil {
		t.Fatal(err)
	}
	intent := ReceiveJournalRoutes{Plans: map[string]ReceiveTransitionPlan{}, Legacy: map[string]ReceiveTransitionPlan{}, Receivers: all}
	for _, p := range plans {
		intent.Plans[p.MachineUID] = p
	}
	for _, legacyCount := range []int{0, 1, 2} {
		intent.Legacy = map[string]ReceiveTransitionPlan{}
		intent.Receivers = map[string]map[string]ReceiveRouteSelection{}
		for node := range all {
			intent.Receivers[node] = map[string]ReceiveRouteSelection{}
		}
		for i := 0; i < legacyCount; i++ {
			intent.Legacy[plans[i].MachineUID] = plans[i]
		}
		if legacyCount < 2 {
			selected, err := PlanReceiveRouteSet(plans[legacyCount:])
			if err != nil {
				t.Fatal(err)
			}
			for node, routes := range selected {
				intent.Receivers[node] = routes
			}
		}
		got, err := PlanReceiveSourceRoutes(intent)
		if err != nil {
			t.Fatal(err)
		}
		if len(got["site"].Hosts) != legacyCount || len(got["retained"].Hosts) != 0 {
			t.Fatalf("mixed phases: %+v", got)
		}
		if legacyCount > 0 && got["site"].Hosts[0] != "10.88.0.128/32" {
			t.Fatal("lost first worker exception")
		}
		got["site"].Addresses[0] = "changed output"
	}
	after, _ := json.Marshal(plans)
	if string(before) != string(after) {
		t.Fatal("projection mutated journals")
	}
	// Removing one source exception from an otherwise valid committed plan must
	// fail admission; callers cannot choose a convenient subset of old paths.
	p := plans[0]
	member := p.Sites[receiverTestKey(16)]
	member.Legacy.Tunnel.Hosts = nil
	p.Sites[receiverTestKey(16)] = member
	if _, err := PlanReceiveRoutes(p); err == nil {
		t.Fatal("incomplete exception set accepted")
	}
}

func TestReceiveSourceRoutesRejectIncompleteAndConflictingIntent(t *testing.T) {
	for _, fault := range []string{"missing-projection", "allocation-conflict", "duplicate-allocation", "legacy-mismatch", "unknown-legacy", "direction-mismatch"} {
		t.Run(fault, func(t *testing.T) {
			plans := sourceRoutePlans(t)
			all, err := PlanReceiveRouteSet(plans)
			if err != nil {
				t.Fatal(err)
			}
			intent := ReceiveJournalRoutes{Plans: map[string]ReceiveTransitionPlan{}, Legacy: map[string]ReceiveTransitionPlan{}, Receivers: all}
			for _, p := range plans {
				intent.Plans[p.MachineUID] = p
			}
			p := plans[1]
			site := p.Sites[receiverTestKey(16)]
			switch fault {
			case "missing-projection":
				site.Legacy.Tunnel = nil
			case "allocation-conflict":
				site.Legacy.Tunnel.Addresses = []string{"10.88.0.10/24", "fd20::10/64"}
				site.Legacy.Tunnel.Hosts = []string{}
			case "duplicate-allocation":
				site.Legacy.Tunnel.Addresses = []string{"10.88.0.9/24", "fd20::9/64"}
				site.Legacy.Tunnel.Hosts = []string{}
			case "legacy-mismatch":
				changed := p
				changed.MachineName = "different"
				intent.Legacy[p.MachineUID] = changed
			case "unknown-legacy":
				intent.Legacy["unknown"] = p
			case "direction-mismatch":
				intent.Legacy[p.MachineUID] = p
			}
			p.Sites[receiverTestKey(16)] = site
			intent.Plans[p.MachineUID] = p
			if _, err := PlanReceiveSourceRoutes(intent); err == nil {
				t.Fatal("unsafe source projection accepted")
			}
		})
	}
}

func TestReceiveSourceRoutesFollowCommittedRollback(t *testing.T) {
	p, tr := journalRouteFixture(t, sourceRoutePlans(t)[0])
	check := func(tr handover.Transition, want int) {
		t.Helper()
		intent, err := PlanReceiveJournalRoutes([]handover.Snapshot{journalRouteSnapshot(t, p, tr)}, "cluster")
		if err != nil {
			t.Fatal(err)
		}
		sources, err := PlanReceiveSourceRoutes(intent)
		if !reflect.DeepEqual(sources, intent.Sources) {
			t.Fatal("journal route admission lost source projection")
		}
		if err != nil || len(sources["site"].Hosts) != want {
			t.Fatalf("committed source routes: %+v %v", sources, err)
		}
	}
	check(tr, 1)
	switched := advanceJournalRoute(t, tr, journalRouteEpoch.Add(time.Second))
	check(switched, 0)
	reverse, err := switched.Rollback(journalRouteEpoch.Add(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	check(reverse, 1)
	aborted, err := tr.Rollback(journalRouteEpoch.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	check(aborted, 1)
}
