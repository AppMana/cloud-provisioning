package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	api "github.com/osrg/gobgp/v3/api"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
)

// A site endpoint's tunnel addresses are what the controller allocated
// for it in each family; a single-stack mesh allocates one.
func TestSiteEndpointTunnelAddressesFollowTheMesh(t *testing.T) {
	secret := &corev1.Secret{Data: map[string][]byte{
		tunnel.NodeTunnelAddressPrefix + "w1":  []byte("10.100.0.1/24"),
		tunnel.NodeTunnelAddress6Prefix + "w1": []byte("fd00:10:100::a64:1/96"),
		tunnel.NodeTunnelAddressPrefix + "w2":  []byte("10.100.0.2/24"),
	}}
	if got, want := siteTunnelAddresses(secret, "w1"), []string{"10.100.0.1/24", "fd00:10:100::a64:1/96"}; !reflect.DeepEqual(got, want) {
		t.Errorf("w1 tunnel addresses = %v, want %v", got, want)
	}
	if got, want := siteTunnelAddresses(secret, "w2"), []string{"10.100.0.2/24"}; !reflect.DeepEqual(got, want) {
		t.Errorf("w2 tunnel addresses = %v, want %v", got, want)
	}
	if got := siteTunnelAddresses(secret, "w3"); len(got) != 0 {
		t.Errorf("an unallocated node has tunnel addresses %v", got)
	}
}

// A remote's identity file names its tunnel address in each family.
func TestRemoteTunnelAddressesComeFromItsIdentity(t *testing.T) {
	doc := tunnel.PeersFileDoc{LocalAddress: "10.100.0.128/24", LocalAddress6: "fd00:10:100::a64:80/96"}
	if got, want := fileTunnelAddresses(doc), []string{"10.100.0.128/24", "fd00:10:100::a64:80/96"}; !reflect.DeepEqual(got, want) {
		t.Errorf("remote tunnel addresses = %v, want %v", got, want)
	}
	doc.LocalAddress6 = ""
	if got, want := fileTunnelAddresses(doc), []string{"10.100.0.128/24"}; !reflect.DeepEqual(got, want) {
		t.Errorf("single-stack remote tunnel addresses = %v, want %v", got, want)
	}
}

// A node forwarding tunnel traffic needs forwarding in every family it
// carries. IPv6 forwarding is only asked for where the tunnel carries
// IPv6: turning it on stops a host accepting router advertisements, and
// a single-stack node has no reason to pay that.
func TestTransitForwardsEachFamilyTheTunnelCarries(t *testing.T) {
	if got, want := forwardingSysctls([]string{"10.100.0.1/24"}), []string{"ipv4/ip_forward"}; !reflect.DeepEqual(got, want) {
		t.Errorf("single-stack forwarding = %v, want %v", got, want)
	}
	if got, want := forwardingSysctls([]string{"10.100.0.1/24", "fd00:10:100::a64:1/96"}), []string{"ipv4/ip_forward", "ipv6/conf/all/forwarding"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dual-stack forwarding = %v, want %v", got, want)
	}
}

// The speaker's routes say "through me", so each route's next hop is
// this node's address of the route's own family. An IPv6 route spoken
// with an IPv4 next hop names a gateway no IPv6 route can use.
func TestTransitSpeaker_EachFamilyHasItsOwnNextHop(t *testing.T) {
	ctx := context.Background()
	speaker, err := startTransitSpeaker(ctx, 17902, 64512, "172.21.0.17,fd8f:cf26:522a::a:11")
	if err != nil {
		t.Fatalf("startTransitSpeaker: %v", err)
	}
	defer speaker.stop(ctx)
	if err := speaker.reconcile(ctx, nil, []transitRoute{
		{prefix: "10.100.0.128/32"}, {prefix: "fd00:10:100::a64:80/128"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for prefix, want := range map[string]string{
		"10.100.0.128/32":         "172.21.0.17",
		"fd00:10:100::a64:80/128": "fd8f:cf26:522a::a:11",
	} {
		got, err := advertisedNextHop(ctx, speaker, prefix)
		if err != nil {
			t.Fatalf("%s: %v", prefix, err)
		}
		if got != want {
			t.Errorf("%s is advertised through %q, want %s", prefix, got, want)
		}
	}
}

// Without an IPv6 address of its own the speaker has no IPv6 route it
// could truthfully say goes through it, and says none.
func TestTransitSpeaker_SpeaksNoIPv6WithoutAnIPv6NextHop(t *testing.T) {
	ctx := context.Background()
	speaker, err := startTransitSpeaker(ctx, 17903, 64512, "172.21.0.17")
	if err != nil {
		t.Fatalf("startTransitSpeaker: %v", err)
	}
	defer speaker.stop(ctx)
	if err := speaker.reconcile(ctx, nil, []transitRoute{
		{prefix: "10.100.0.128/32"}, {prefix: "fd00:10:100::a64:80/128"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := advertisedNextHop(ctx, speaker, "fd00:10:100::a64:80/128"); err == nil {
		t.Error("an IPv6 route was advertised by a speaker with no IPv6 next hop")
	}
	if got, err := advertisedNextHop(ctx, speaker, "10.100.0.128/32"); err != nil || got != "172.21.0.17" {
		t.Errorf("the IPv4 route = %q, %v", got, err)
	}
}

// advertisedNextHop reads a prefix's path from the speaker's own table
// and returns the next hop it carries, however the family encodes it.
func advertisedNextHop(ctx context.Context, t *transitSpeaker, prefix string) (string, error) {
	family := &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}
	if strings.Contains(prefix, ":") {
		family = &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST}
	}
	nextHop, found := "", false
	err := t.server.ListPath(ctx, &api.ListPathRequest{TableType: api.TableType_GLOBAL, Family: family}, func(d *api.Destination) {
		if d.Prefix != prefix {
			return
		}
		for _, p := range d.Paths {
			for _, attr := range p.Pattrs {
				nh := &api.NextHopAttribute{}
				if attr.UnmarshalTo(nh) == nil {
					nextHop, found = nh.NextHop, true
				}
				mp := &api.MpReachNLRIAttribute{}
				if attr.UnmarshalTo(mp) == nil && len(mp.NextHops) > 0 {
					nextHop, found = mp.NextHops[0], true
				}
			}
		}
	})
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("no path for %s", prefix)
	}
	return nextHop, nil
}
