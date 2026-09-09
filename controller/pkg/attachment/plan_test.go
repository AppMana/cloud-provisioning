package attachment

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

func observedRequest() GatewayRequest {
	subnet := netip.MustParsePrefix("172.29.0.0/25")
	return GatewayRequest{
		Worker:            Machine{UID: "windows-machine-uid", NodeUID: "windows-node-uid", InterfaceID: "eni-worker", ProviderID: "aws:///us-west-2a/i-worker", NetworkID: "aws:270666084746:us-west-2:vpc-test", Subnet: subnet, Address: netip.MustParseAddr("172.29.0.21")},
		Gateway:           Machine{UID: "relay-machine-uid", NodeUID: "relay-node-uid", InterfaceID: "eni-relay", ProviderID: "aws:///us-west-2a/i-relay", NetworkID: "aws:270666084746:us-west-2:vpc-test", Subnet: subnet, Address: netip.MustParseAddr("172.29.0.9")},
		SiteTransport:     []netip.Addr{netip.MustParseAddr("10.10.0.11")},
		SiteReturnSources: []netip.Addr{netip.MustParseAddr("10.100.0.1")},
		Underlay:          []netip.Addr{netip.MustParseAddr("16.146.98.223"), netip.MustParseAddr("18.237.90.169")}, UDPPort: 4789,
	}
}

func TestControlPlanePortsAreCanonicalAndChangeAttachmentIdentity(t *testing.T) {
	r := observedRequest()
	old, err := PlanGateway(r)
	if err != nil {
		t.Fatal(err)
	}
	r.TCPPorts = []uint16{8132, 6443, 8132}
	plan, err := PlanGateway(r)
	if err != nil || !reflect.DeepEqual(plan.TCPPorts, []uint16{6443, 8132}) {
		t.Fatalf("ports not canonical: %+v %v", plan, err)
	}
	if planDigest(old) == planDigest(plan) {
		t.Fatal("adding Konnectivity must retire the old immutable binding")
	}
	r.TCPPorts = []uint16{6443, 8132}
	reordered, err := PlanGateway(r)
	if err != nil || planDigest(reordered) != planDigest(plan) {
		t.Fatal("equivalent port sets changed attachment identity")
	}
	r.TCPPorts[0] = 0
	if _, err := PlanGateway(r); err == nil {
		t.Fatal("accepted TCP port zero")
	}
}

// The small-pod HTTP feasibility experiment needed both observed source
// addresses. Using only NodeInternalIP passed neither traffic direction.
func TestObservedNativeEthernetPath(t *testing.T) {
	for _, worker := range []string{"172.29.0.21", "172.29.0.13"} {
		r := observedRequest()
		r.Worker.Address = netip.MustParseAddr(worker)
		r.SiteTransport = append(r.SiteTransport, r.SiteTransport[0])
		plan, err := PlanGateway(r)
		if err != nil {
			t.Fatal(err)
		}
		want := []netip.Prefix{netip.MustParsePrefix("10.10.0.11/32"), netip.MustParsePrefix("10.100.0.1/32")}
		if !reflect.DeepEqual(plan.ReturnHosts, want) || !reflect.DeepEqual(plan.DirectHosts, []netip.Prefix{netip.MustParsePrefix("10.10.0.11/32")}) || plan.WorkerHost.String() != worker+"/32" {
			t.Fatalf("incorrect packet path: %+v", plan)
		}
		plan.DirectHosts[0] = netip.MustParsePrefix("192.0.2.1/32")
		if !reflect.DeepEqual(plan.ReturnHosts, want) {
			t.Fatal("guest route changes mutated provider return routes")
		}
	}
}

func TestGatewayRejectsUnresolvedIdentityAndRoutingLoops(t *testing.T) {
	cases := map[string]func(*GatewayRequest){
		"missing Node identity":             func(r *GatewayRequest) { r.Worker.NodeUID = "" },
		"missing network interface":         func(r *GatewayRequest) { r.Gateway.InterfaceID = "" },
		"same Node identity":                func(r *GatewayRequest) { r.Gateway.NodeUID = r.Worker.NodeUID },
		"same interface":                    func(r *GatewayRequest) { r.Gateway.InterfaceID = r.Worker.InterfaceID },
		"missing worker UID":                func(r *GatewayRequest) { r.Worker.UID = "" },
		"missing gateway provider identity": func(r *GatewayRequest) { r.Gateway.ProviderID = "" },
		"same machine UID":                  func(r *GatewayRequest) { r.Gateway.UID = r.Worker.UID },
		"same provider identity":            func(r *GatewayRequest) { r.Gateway.ProviderID = r.Worker.ProviderID },
		"same native address":               func(r *GatewayRequest) { r.Gateway.Address = r.Worker.Address },
		"different network":                 func(r *GatewayRequest) { r.Gateway.NetworkID = "another-provider:another-vpc" },
		"different subnet":                  func(r *GatewayRequest) { r.Gateway.Subnet = netip.MustParsePrefix("172.29.0.0/24") },
		"noncanonical subnet":               func(r *GatewayRequest) { r.Gateway.Subnet = netip.MustParsePrefix("172.29.0.9/25") },
		"address outside subnet":            func(r *GatewayRequest) { r.Worker.Address = netip.MustParseAddr("172.30.0.1") },
		"return source intercepts underlay": func(r *GatewayRequest) { r.SiteReturnSources = r.Underlay },
		"return source intercepts subnet":   func(r *GatewayRequest) { r.SiteReturnSources = []netip.Addr{r.Worker.Address} },
		"no site transport":                 func(r *GatewayRequest) { r.SiteTransport = nil },
		"no underlay observation":           func(r *GatewayRequest) { r.Underlay = nil },
		"unknown protocol":                  func(r *GatewayRequest) { r.UDPPort = 0 },
		"outer tunnel would loop":           func(r *GatewayRequest) { r.SiteTransport = append(r.SiteTransport, r.Underlay[0]) },
		"native address is tunnel endpoint": func(r *GatewayRequest) { r.Underlay = append(r.Underlay, r.Worker.Address) },
		"intercept cloud subnet":            func(r *GatewayRequest) { r.SiteTransport = append(r.SiteTransport, r.Gateway.Address) },
		"link local":                        func(r *GatewayRequest) { r.SiteTransport = []netip.Addr{netip.MustParseAddr("169.254.169.254")} },
		"loopback":                          func(r *GatewayRequest) { r.SiteTransport = []netip.Addr{netip.MustParseAddr("127.0.0.1")} },
		"multicast underlay":                func(r *GatewayRequest) { r.Underlay = []netip.Addr{netip.MustParseAddr("224.0.0.1")} },
		"unqualified IPv6":                  func(r *GatewayRequest) { r.SiteTransport = []netip.Addr{netip.MustParseAddr("fd00::1")} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := observedRequest()
			mutate(&r)
			if _, err := PlanGateway(r); err == nil {
				t.Fatal("accepted invalid attachment")
			}
		})
	}
}

func TestGatewayPeerProjectionPreservesOwnershipAndWorkerEthernetRoutes(t *testing.T) {
	plan, err := PlanGateway(observedRequest())
	if err != nil {
		t.Fatal(err)
	}
	peers := []tunnel.PeerSpec{
		{PublicKey: "site", WGAllowedIPs: []string{"10.100.0.1/32", "10.10.0.11/32", "10.10.0.10/32"}, RouteHost: "10.100.0.1", RouteHosts: []string{"10.10.0.11/32", "10.10.0.10"}, TransitHosts: []string{"10.10.0.11", "10.10.0.10"}},
		{PublicKey: "gateway", WGAllowedIPs: []string{"10.100.0.136/32"}, RouteHosts: []string{"10.100.0.136"}},
	}
	before, _ := json.Marshal(peers)
	site, err := GatewayPeers(plan, peers, "gateway", false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(site[1].WGAllowedIPs, []string{"10.100.0.136/32", "172.29.0.21/32"}) {
		t.Fatalf("gateway did not acquire native host: %+v", site[1])
	}
	again, err := GatewayPeers(plan, site, "gateway", false)
	if err != nil || !reflect.DeepEqual(site, again) {
		t.Fatalf("projection is not idempotent: %v", err)
	}
	worker, err := GatewayPeers(plan, peers, "gateway", true)
	if err != nil {
		t.Fatal(err)
	}
	if worker[0].RouteHost != "" || !reflect.DeepEqual(worker[0].RouteHosts, []string{"10.10.0.10", "10.100.0.1"}) || !reflect.DeepEqual(worker[0].TransitHosts, []string{"10.10.0.10"}) {
		t.Fatalf("worker must suppress native CNI routes and preserve API mesh routes: %+v", worker[0])
	}
	if !reflect.DeepEqual(worker[0].WGAllowedIPs, peers[0].WGAllowedIPs) {
		t.Fatal("guest route selection widened the cryptographic accept list")
	}
	after, _ := json.Marshal(peers)
	if string(before) != string(after) {
		t.Fatal("projection mutated its input")
	}
	for _, key := range []string{"", "missing"} {
		if _, err := GatewayPeers(plan, peers, key, false); err == nil {
			t.Fatal("unavailable gateway accepted")
		}
	}
	for _, prefix := range []string{"172.29.0.21/32", "172.29.0.0/25", "invalid"} {
		bad := append([]tunnel.PeerSpec(nil), peers...)
		bad[0].WGAllowedIPs = []string{prefix}
		if _, err := GatewayPeers(plan, bad, "gateway", false); err == nil {
			t.Fatalf("stole existing prefix %s", prefix)
		}
	}
	duplicate := append(peers, peers[1])
	if _, err := GatewayPeers(plan, duplicate, "gateway", false); err == nil {
		t.Fatal("duplicate gateway key accepted")
	}
	plan.WorkerHost = netip.Prefix{}
	if _, err := GatewayPeers(plan, peers, "gateway", false); err == nil {
		t.Fatal("invalid worker prefix accepted")
	}
}
