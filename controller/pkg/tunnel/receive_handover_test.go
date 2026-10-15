package tunnel

import (
	"encoding/base64"
	"net/netip"
	"reflect"
	"testing"
)

func receiverTestKey(value byte) string {
	var k [32]byte
	k[0] = value
	return base64.StdEncoding.EncodeToString(k[:])
}

func TestReceivePreparationPreservesBothOwnersWithoutChangingEgress(t *testing.T) {
	a, b, c := receiverTestKey(8), receiverTestKey(16), receiverTestKey(24)
	before := PeerListDoc{Peers: []PeerSpec{{PublicKey: a, WGAllowedIPs: []string{"10.90.0.10/32", "2001:db8:1::10/128"}}}}
	after := PeerListDoc{Peers: []PeerSpec{{PublicKey: b, WGAllowedIPs: []string{"10.90.0.10/32", "2001:db8:1::10/128"}, RouteHosts: []string{"10.90.0.10"}}, {PublicKey: c, Remote: true, WGAllowedIPs: []string{"10.100.0.129/32"}}}, APIServers: []string{"10.90.0.1:6443"}}
	plan, err := PlanReceiveHandover("mesh-a", before, after, []netip.Prefix{netip.MustParsePrefix("172.29.0.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Receivers) != 2 || !reflect.DeepEqual(plan.Desired, after) {
		t.Fatalf("preparation changed cloud peers or egress: %+v", plan)
	}
	for _, path := range plan.Receivers {
		if path.PublicKey == c || !reflect.DeepEqual(path.AllowedSources, before.Peers[0].WGAllowedIPs) {
			t.Fatal("source authorization was widened or native peer became a receiver")
		}
		if len(path.Interface) > 15 {
			t.Fatal("invalid kernel interface name")
		}
	}
	if plan.Receivers[0].Interface == plan.Receivers[1].Interface {
		t.Fatal("receiver paths share one accept trie")
	}
	plan.Desired.Peers[0].WGAllowedIPs[0] = "changed"
	if after.Peers[0].WGAllowedIPs[0] != "10.90.0.10/32" {
		t.Fatal("plan aliases the controller document")
	}
	stable, err := PlanReceiveHandover("mesh-a", after, after, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stable.Receivers) != 1 {
		t.Fatal("stable generation retained departed permissions")
	}
	for _, path := range plan.Receivers {
		if path.PublicKey == b && path.Interface != stable.Receivers[0].Interface {
			t.Fatal("surviving receiver identity changed")
		}
	}
}

func TestReceivePreparationRejectsUnsafeSourceOwnership(t *testing.T) {
	a, b := receiverTestKey(8), receiverTestKey(16)
	for _, test := range []struct {
		name   string
		peers  []PeerSpec
		native []netip.Prefix
	}{
		{"overlapping owners", []PeerSpec{{PublicKey: a, WGAllowedIPs: []string{"10.90.0.0/24"}}, {PublicKey: b, WGAllowedIPs: []string{"10.90.0.10/32"}}}, nil},
		{"duplicate identity", []PeerSpec{{PublicKey: a}, {PublicKey: a}}, nil},
		{"default", []PeerSpec{{PublicKey: a, WGAllowedIPs: []string{"0.0.0.0/0"}}}, nil},
		{"noncanonical", []PeerSpec{{PublicKey: a, WGAllowedIPs: []string{"10.90.0.1/24"}}}, nil},
		{"mapped", []PeerSpec{{PublicKey: a, WGAllowedIPs: []string{"::ffff:10.90.0.1/128"}}}, nil},
		{"native ipv4", []PeerSpec{{PublicKey: a, WGAllowedIPs: []string{"172.29.0.10/32"}}}, []netip.Prefix{netip.MustParsePrefix("172.29.0.0/24")}},
		{"native ipv6", []PeerSpec{{PublicKey: a, WGAllowedIPs: []string{"2001:db8:2::/64"}}}, []netip.Prefix{netip.MustParsePrefix("2001:db8:2::/80")}},
		{"missing key", []PeerSpec{{WGAllowedIPs: []string{"10.90.0.1/32"}}}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := PlanReceiveHandover("mesh", PeerListDoc{Peers: []PeerSpec{}}, PeerListDoc{Peers: test.peers}, test.native); err == nil {
				t.Fatal("unsafe receive plan accepted")
			}
		})
	}
	if _, err := PlanReceiveHandover("mesh", PeerListDoc{Peers: []PeerSpec{{PublicKey: a}}}, PeerListDoc{Peers: []PeerSpec{{PublicKey: a, Remote: true}}}, nil); err == nil {
		t.Fatal("identity changed trust role")
	}
}

func TestReceivePreparationDistinguishesEmptyFromMissingState(t *testing.T) {
	empty := PeerListDoc{Peers: []PeerSpec{}}
	if _, err := PlanReceiveHandover("mesh", empty, empty, nil); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]PeerListDoc{{{}, empty}, {empty, {}}} {
		if _, err := PlanReceiveHandover("mesh", pair[0], pair[1], nil); err == nil {
			t.Fatal("missing document treated as authoritative empty state")
		}
	}
}

func TestReceivePreparationValidatesCloudOwnership(t *testing.T) {
	a, b := receiverTestKey(8), receiverTestKey(16)
	empty := PeerListDoc{Peers: []PeerSpec{}}
	for _, test := range []struct {
		name  string
		peers []PeerSpec
	}{
		{"site before cloud", []PeerSpec{{PublicKey: a, WGAllowedIPs: []string{"10.90.0.0/24"}}, {PublicKey: b, Remote: true, WGAllowedIPs: []string{"10.90.0.10/32"}}}},
		{"cloud before site", []PeerSpec{{PublicKey: b, Remote: true, WGAllowedIPs: []string{"10.90.0.0/24"}}, {PublicKey: a, WGAllowedIPs: []string{"10.90.0.10/32"}}}},
		{"cloud owners", []PeerSpec{{PublicKey: a, Remote: true, WGAllowedIPs: []string{"2001:db8:1::/64"}}, {PublicKey: b, Remote: true, WGAllowedIPs: []string{"2001:db8:1::10/128"}}}},
		{"malformed cloud prefix", []PeerSpec{{PublicKey: a, Remote: true, WGAllowedIPs: []string{"invalid"}}}},
		{"cloud default route", []PeerSpec{{PublicKey: a, Remote: true, WGAllowedIPs: []string{"::/0"}}}},
		{"cloud host bits", []PeerSpec{{PublicKey: a, Remote: true, WGAllowedIPs: []string{"172.29.0.10/24"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := PeerListDoc{Peers: test.peers}
			for _, pair := range [][2]PeerListDoc{{empty, invalid}, {invalid, empty}} {
				plan, err := PlanReceiveHandover("mesh", pair[0], pair[1], nil)
				if err == nil {
					t.Fatal("unsafe cloud ownership accepted")
				}
				if !reflect.DeepEqual(plan, ReceiveHandover{}) {
					t.Fatal("rejected input returned a partial plan")
				}
			}
		})
	}
}

func TestReceivePreparationPreservesProtectedCloudPeers(t *testing.T) {
	doc := PeerListDoc{Peers: []PeerSpec{
		{PublicKey: receiverTestKey(8), WGAllowedIPs: []string{"10.90.0.10/32"}},
		{PublicKey: receiverTestKey(16), Remote: true, WGAllowedIPs: []string{"172.29.0.10/32", "2001:db8:2::10/128"}},
	}}
	native := []netip.Prefix{netip.MustParsePrefix("172.29.0.0/24"), netip.MustParsePrefix("2001:db8:2::/80")}
	plan, err := PlanReceiveHandover("mesh", doc, doc, native)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Desired, doc) || len(plan.Receivers) != 1 || plan.Receivers[0].PublicKey != doc.Peers[0].PublicKey {
		t.Fatal("protected cloud peer changed or became a site receiver")
	}
}

func TestReceivePreparationRejectsCrossGenerationTrustTransfer(t *testing.T) {
	for _, prefixes := range [][2]string{{"10.90.0.0/24", "10.90.0.10/32"}, {"2001:db8:1::/64", "2001:db8:1::10/128"}} {
		site := PeerListDoc{Peers: []PeerSpec{{PublicKey: receiverTestKey(8), WGAllowedIPs: []string{prefixes[0]}}}}
		cloud := PeerListDoc{Peers: []PeerSpec{{PublicKey: receiverTestKey(16), Remote: true, WGAllowedIPs: []string{prefixes[1]}}}}
		for _, pair := range [][2]PeerListDoc{{site, cloud}, {cloud, site}} {
			if _, err := PlanReceiveHandover("mesh", pair[0], pair[1], nil); err == nil {
				t.Fatal("overlapping generations transferred site/cloud source authority")
			}
		}
	}
}
