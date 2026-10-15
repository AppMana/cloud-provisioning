package tunnel

import (
	"reflect"
	"testing"
)

func TestSiteReceiveClientPreservesSourceUnionAndSeparatesIdentity(t *testing.T) {
	site, root, receiver := receiverTestKey(8), receiverTestKey(16), receiverTestKey(24)
	binding := ReceiveBinding{PeerPublicKey: site, PublicKey: receiver, ListenPort: 51821}
	before := PeerListDoc{Peers: []PeerSpec{{PublicKey: root, Remote: true, Endpoint: "198.51.100.10:51820", WGAllowedIPs: []string{"172.29.0.10/32"}}}}
	after := PeerListDoc{Peers: []PeerSpec{{PublicKey: root, Remote: true, Endpoint: "198.51.100.10:51820", WGAllowedIPs: []string{"172.29.0.11/32", "2001:db8:2::10/128"}}}}
	plan, err := PlanSiteReceiveClient("mesh", site, root, binding, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PublicKey != receiver || plan.Endpoint != "198.51.100.10:51821" || plan.CloudRootPublicKey != root || len(plan.Interface) > 15 || !reflect.DeepEqual(plan.AllowedSources, []string{"172.29.0.10/32", "172.29.0.11/32", "2001:db8:2::10/128"}) {
		t.Fatal("site receive plan lost permissions or used the legacy cloud transport")
	}
	stable, err := PlanSiteReceiveClient("mesh", site, root, binding, after, after)
	if err != nil {
		t.Fatal(err)
	}
	if stable.Interface != plan.Interface {
		t.Fatal("source update changed client device identity")
	}
	after.Peers[0].Endpoint = "[2001:db8:3::10]:51820"
	ipv6, err := PlanSiteReceiveClient("mesh", site, root, binding, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if ipv6.Endpoint != "[2001:db8:3::10]:51821" {
		t.Fatal("IPv6 endpoint was not preserved")
	}
}

func TestSiteReceiveClientRejectsUnboundIdentitiesAndMissingState(t *testing.T) {
	for _, fault := range []string{"site-key", "root-key", "binding-site", "binding-root", "binding-local", "port", "not-cloud", "endpoint", "default", "missing-previous", "missing-desired"} {
		t.Run(fault, func(t *testing.T) {
			site, root := receiverTestKey(8), receiverTestKey(16)
			binding := ReceiveBinding{PeerPublicKey: site, PublicKey: receiverTestKey(24), ListenPort: 51821}
			before := PeerListDoc{Peers: []PeerSpec{}}
			after := PeerListDoc{Peers: []PeerSpec{{PublicKey: root, Remote: true, Endpoint: "198.51.100.10:51820", WGAllowedIPs: []string{"172.29.0.10/32"}}}}
			switch fault {
			case "site-key":
				site = "invalid"
			case "root-key":
				root = "invalid"
			case "binding-site":
				binding.PeerPublicKey = receiverTestKey(32)
			case "binding-root":
				binding.PublicKey = root
			case "binding-local":
				binding.PublicKey = site
			case "port":
				binding.ListenPort = 0
			case "not-cloud":
				after.Peers[0].Remote = false
			case "endpoint":
				after.Peers[0].Endpoint = ""
			case "default":
				after.Peers[0].WGAllowedIPs = []string{"0.0.0.0/0"}
			case "missing-previous":
				before.Peers = nil
			case "missing-desired":
				after.Peers = []PeerSpec{}
			}
			if _, err := PlanSiteReceiveClient("mesh", site, root, binding, before, after); err == nil {
				t.Fatal("unsafe site client plan accepted")
			}
		})
	}
}
