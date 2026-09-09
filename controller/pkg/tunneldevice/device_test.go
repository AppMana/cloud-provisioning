package tunneldevice

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func fixture(t *testing.T) tunnel.PeersFileDoc {
	t.Helper()
	private, e := wgtypes.GeneratePrivateKey()
	if e != nil {
		t.Fatal(e)
	}
	remote, e := wgtypes.GeneratePrivateKey()
	if e != nil {
		t.Fatal(e)
	}
	return tunnel.PeersFileDoc{PrivateKey: private.String(), LocalAddress: "10.254.0.1/32", Peers: []tunnel.PeerSpec{{PublicKey: remote.PublicKey().String(), Endpoint: "192.0.2.1:51820", WGAllowedIPs: []string{"10.254.0.2/32", "10.244.0.0/24"}, RouteHosts: []string{"10.254.0.2/32"}}}}
}
func TestPodPrefixesNeverBecomeRoutes(t *testing.T) {
	d := fixture(t)
	p, e := Compile(d)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Routes) != 1 || p.Routes[0].String() != "10.254.0.2/32" || len(p.Peers[0].Allowed) != 2 {
		t.Fatalf("unexpected routing plan: %v", p.Routes)
	}
}
func TestRejectInvalidPeerDocuments(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*tunnel.PeersFileDoc)
	}{
		{"bad key", func(d *tunnel.PeersFileDoc) { d.PrivateKey = "not a key" }},
		{"wide local address", func(d *tunnel.PeersFileDoc) { d.LocalAddress = "10.254.0.1/24" }},
		{"wide route", func(d *tunnel.PeersFileDoc) { d.Peers[0].RouteHosts = []string{"10.244.0.0/24"} }},
		{"unpermitted route", func(d *tunnel.PeersFileDoc) { d.Peers[0].RouteHosts = []string{"10.9.0.1/32"} }},
		{"bad endpoint", func(d *tunnel.PeersFileDoc) { d.Peers[0].Endpoint = "example.com:51820" }},
		{"duplicate key", func(d *tunnel.PeersFileDoc) { d.Peers = append(d.Peers, d.Peers[0]) }},
		{"noncanonical prefix", func(d *tunnel.PeersFileDoc) { d.Peers[0].WGAllowedIPs = []string{"10.244.0.1/24"} }},
		{"duplicate prefix", func(d *tunnel.PeersFileDoc) {
			d.Peers[0].WGAllowedIPs = append(d.Peers[0].WGAllowedIPs, "10.254.0.2/32")
		}},
		{"self route", func(d *tunnel.PeersFileDoc) { d.Peers[0].RouteHosts = []string{d.LocalAddress} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := fixture(t)
			test.change(&d)
			if _, e := Compile(d); e == nil {
				t.Fatal("unsafe document accepted")
			}
		})
	}
}
func TestIPv6AndLegacyRoute(t *testing.T) {
	d := fixture(t)
	d.LocalAddress = "fd00::1/128"
	d.Peers[0].Endpoint = "[2001:db8::1]:51820"
	d.Peers[0].WGAllowedIPs = []string{"fd00::2/128"}
	d.Peers[0].RouteHosts = nil
	d.Peers[0].RouteHost = "fd00::2/128"
	p, e := Compile(d)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Routes) != 1 || !strings.Contains(p.Routes[0].String(), "/128") {
		t.Fatal(p.Routes)
	}
	d.Peers = nil
	p, e = Compile(d)
	if e != nil || len(p.Peers) != 0 || len(p.Routes) != 0 {
		t.Fatal("cannot remove all peers", e)
	}
}

// The live CAPI peer document uses bare host addresses, as RemotePeers has
// always published. Both native and prefix forms must describe host routes.
func TestBareRouteHostsFromControllerPublication(t *testing.T) {
	d := fixture(t)
	d.Peers[0].WGAllowedIPs = append(d.Peers[0].WGAllowedIPs, "fd00::2/128")
	d.Peers[0].RouteHosts = []string{"10.254.0.2", "fd00::2"}
	p, err := Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Routes) != 2 || p.Routes[0].String() != "10.254.0.2/32" || p.Routes[1].String() != "fd00::2/128" {
		t.Fatal(p.Routes)
	}
	d.Peers[0].RouteHosts = []string{"10.254.0.1"}
	if _, err := Compile(d); err == nil {
		t.Fatal("bare self route accepted")
	}
	d.Peers[0].RouteHosts = []string{"192.0.2.90"}
	if _, err := Compile(d); err == nil {
		t.Fatal("unpermitted bare route accepted")
	}
}

func TestKernelRoutesRejectMissingOrStaleOwnedRoutes(t *testing.T) {
	wanted := netip.MustParsePrefix("10.10.0.10/32")
	old := netip.MustParsePrefix("10.10.0.13/32")
	multicast := netip.MustParsePrefix("224.0.0.0/4")
	owned := map[netip.Prefix]bool{wanted: true, old: true}
	if err := VerifyRoutes([]netip.Prefix{wanted}, owned, []netip.Prefix{multicast}); err == nil {
		t.Fatal("missing API route accepted")
	}
	if err := VerifyRoutes([]netip.Prefix{wanted}, owned, []netip.Prefix{wanted, old}); err == nil {
		t.Fatal("retired owned route accepted")
	}
	if err := VerifyRoutes([]netip.Prefix{wanted}, owned, []netip.Prefix{wanted, multicast}); err != nil {
		t.Fatal(err)
	}
}
