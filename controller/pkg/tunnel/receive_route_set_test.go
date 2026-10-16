package tunnel

import (
	"encoding/json"
	"reflect"
	"testing"
)

func routeSetFixture(t *testing.T) []ReceiveTransitionPlan {
	t.Helper()
	first, second := routeTransitionFixture(t), routeTransitionFixture(t)
	first.Namespace, first.MeshName, first.MachineUID, first.CloudNodeName = "namespace", "mesh-name", "machine-one", "cloud-node"
	second.Namespace, second.MeshName, second.MachineUID, second.CloudNodeName, second.CloudNodeUID = "namespace", "mesh-name", "machine-two", "cloud-two-node", "cloud-two"
	second.RootPublicKey = receiverTestKey(80)
	second.SiteDesired.Peers[0].PublicKey = second.RootPublicKey
	second.SiteDesired.Peers[0].WGAllowedIPs = []string{"172.29.0.3/32", "2001:db8:2::3/128"}
	second.SiteDesired.Peers[0].RouteHosts = []string{"172.29.0.3"}
	for i, key := range []string{receiverTestKey(8), receiverTestKey(16)} {
		binding := second.Bindings[key]
		binding.PublicKey = receiverTestKey(byte(96 + 8*i))
		second.Bindings[key] = binding
	}
	refreshRouteSetClients(t, &second)
	return []ReceiveTransitionPlan{first, second}
}
func refreshRouteSetClients(t *testing.T, p *ReceiveTransitionPlan) {
	t.Helper()
	for key, site := range p.Sites {
		var err error
		site.Client, err = PlanSiteReceiveClient(p.MeshUID, key, p.RootPublicKey, p.Bindings[key], p.SiteDesired, p.SiteDesired)
		if err != nil {
			t.Fatal(err)
		}
		p.Sites[key] = site
	}
}
func TestReceiveRouteSetPreservesIndependentWorkers(t *testing.T) {
	plans := routeSetFixture(t)
	before, _ := json.Marshal(plans)
	combined, err := PlanReceiveRouteSet(plans)
	if err != nil {
		t.Fatal(err)
	}
	if len(combined) != 4 {
		t.Fatalf("lost cloud or retained site node: %v", combined)
	}
	for _, node := range []string{"site", "retained"} {
		if len(combined[node]) != 4 {
			t.Fatalf("lost independent cloud destinations on %s", node)
		}
		for _, prefix := range []string{"172.29.0.1/32", "2001:db8:2::1/128", "172.29.0.3/32", "2001:db8:2::3/128"} {
			if _, ok := combined[node][prefix]; !ok {
				t.Fatal("missing", prefix)
			}
		}
	}
	reversed, err := PlanReceiveRouteSet([]ReceiveTransitionPlan{plans[1], plans[0]})
	if err != nil || !reflect.DeepEqual(combined, reversed) {
		t.Fatalf("ordering changed route set: %v", err)
	}
	for _, p := range plans {
		one, err := PlanReceiveRoutes(p)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(combined[p.CloudNodeUID], one[p.CloudNodeUID]) {
			t.Fatal("cloud routes changed during aggregation")
		}
	}
	selection := combined["site"]["172.29.0.1/32"]
	selection.AllowedSources[0] = "changed-output"
	after, _ := json.Marshal(plans)
	if string(before) != string(after) {
		t.Fatal("aggregate mutated input plans")
	}
}
func TestReceiveRouteSetRejectsCrossJournalConflicts(t *testing.T) {
	for _, fault := range []string{"namespace", "mesh", "machine", "cloud-node", "site-incarnation", "site-name", "root-key", "receiver-key", "ipv4-overlap", "ipv6-overlap", "nested-overlap"} {
		t.Run(fault, func(t *testing.T) {
			plans := routeSetFixture(t)
			first, second := &plans[0], &plans[1]
			switch fault {
			case "namespace":
				second.Namespace = "other"
			case "mesh":
				second.MeshName = "other"
			case "machine":
				second.MachineUID = first.MachineUID
			case "cloud-node":
				second.CloudNodeUID = first.CloudNodeUID
			case "site-incarnation":
				site := second.Sites[receiverTestKey(16)]
				site.NodeUID = "replaced-site"
				second.Sites[receiverTestKey(16)] = site
			case "site-name":
				site := second.Sites[receiverTestKey(16)]
				site.NodeName = "renamed-site"
				second.Sites[receiverTestKey(16)] = site
			case "root-key":
				second.RootPublicKey = first.RootPublicKey
				second.SiteDesired.Peers[0].PublicKey = first.RootPublicKey
			case "receiver-key":
				key := receiverTestKey(16)
				binding := second.Bindings[key]
				binding.PublicKey = first.Bindings[key].PublicKey
				second.Bindings[key] = binding
			case "ipv4-overlap":
				second.SiteDesired.Peers[0].WGAllowedIPs[0] = "172.29.0.1/32"
				second.SiteDesired.Peers[0].RouteHosts = []string{"172.29.0.1"}
			case "ipv6-overlap":
				second.SiteDesired.Peers[0].WGAllowedIPs[1] = "2001:db8:2::1/128"
			case "nested-overlap":
				second.SiteDesired.Peers[0].WGAllowedIPs[1] = "2001:db8:2::/64"
			}
			refreshRouteSetClients(t, second)
			// Each journal remains independently executable configuration; rejection
			// must come from combining its identities or destinations with the other.
			for _, p := range plans {
				if _, err := PlanReceiveRoutes(p); err != nil {
					t.Fatalf("fault invalidated individual plan: %v", err)
				}
			}
			if got, err := PlanReceiveRouteSet(plans); err == nil || got != nil {
				t.Fatal("conflicting journals produced an executable route set")
			}
		})
	}
	if got, err := PlanReceiveRouteSet(nil); err == nil || got != nil {
		t.Fatal("empty input authorized route withdrawal")
	}
}
