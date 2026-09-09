package tunnel

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"testing"
)

func TestGatewayWireProjectionMatchesSiteAndWorkerRoles(t *testing.T) {
	p := GatewayProjection{Lease: "worker/epoch", WorkerKey: "worker", GatewayKey: "gateway", WorkerAddress: netip.MustParseAddr("172.29.0.21"), WorkerHost: netip.MustParsePrefix("172.29.0.21/32"), DirectHosts: []netip.Prefix{netip.MustParsePrefix("10.10.0.11/32")}}
	raw, _ := json.Marshal([]GatewayProjection{p})
	data := map[string][]byte{GatewayProjectionsKey: raw}
	peers := []PeerSpec{{PublicKey: "gateway", WGAllowedIPs: []string{"10.100.0.136/32"}, RouteHosts: []string{"10.100.0.136"}}, {PublicKey: "site", WGAllowedIPs: []string{"10.10.0.11/32"}, RouteHosts: []string{"10.10.0.11"}}}
	baseline, _ := json.Marshal(peers)
	site, err := ProjectGateways(data, peers, "")
	if err != nil {
		t.Fatal(err)
	}
	if !containsHost(site[0].RouteHosts, "172.29.0.21") {
		t.Fatal("site did not route native worker through gateway")
	}
	worker, err := ProjectGateways(data, peers, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(worker[1].AllRouteHosts()) != 0 || len(worker[1].WGAllowedIPs) != 1 {
		t.Fatal("worker route suppression changed cryptographic acceptance")
	}
	gateway, err := ProjectGateways(data, peers, "gateway")
	if err != nil || !reflect.DeepEqual(gateway, peers) {
		t.Fatal("gateway tried routing through itself")
	}
	again, err := ProjectGateways(data, site, "")
	if err != nil || !reflect.DeepEqual(again, site) {
		t.Fatal("projection not idempotent")
	}
	after, _ := json.Marshal(peers)
	if string(after) != string(baseline) {
		t.Fatal("projection mutated baseline")
	}
	// Reproduces a slow retiring worker: site recipients can apply first.
	// Keep their native return path while giving the worker its API routes.
	p.RetiringWorker = true
	data[GatewayProjectionsKey], _ = json.Marshal([]GatewayProjection{p})
	stagedSite, err := ProjectGateways(data, peers, "")
	if err != nil || !reflect.DeepEqual(stagedSite, site) {
		t.Fatal("worker preparation withdrew the site return path", err)
	}
	stagedWorker, err := ProjectGateways(data, peers, "worker")
	if err != nil || !reflect.DeepEqual(stagedWorker, peers) {
		t.Fatal("worker preparation did not restore direct routes", err)
	}
	delete(data, GatewayProjectionsKey)
	retired, err := ProjectGateways(data, peers, "")
	if err != nil || !reflect.DeepEqual(retired, peers) {
		t.Fatal("withdrawal did not restore source render")
	}
}
func TestGatewayProjectionRejectsConflictingOwnership(t *testing.T) {
	p := GatewayProjection{Lease: "epoch", WorkerKey: "worker", GatewayKey: "gateway", WorkerAddress: netip.MustParseAddr("172.29.0.21"), WorkerHost: netip.MustParsePrefix("172.29.0.21/32")}
	raw, _ := json.Marshal([]GatewayProjection{p, p})
	if _, err := ProjectGateways(map[string][]byte{GatewayProjectionsKey: raw}, nil, ""); err == nil {
		t.Fatal("duplicate attachment accepted")
	}
	raw, _ = json.Marshal([]GatewayProjection{p})
	peers := []PeerSpec{{PublicKey: "gateway"}, {PublicKey: "foreign", WGAllowedIPs: []string{"172.29.0.0/24"}}}
	if _, err := ProjectGateways(map[string][]byte{GatewayProjectionsKey: raw}, peers, ""); err == nil {
		t.Fatal("overlapping ownership accepted")
	}
}

func TestGatewayProjectionReachesSiteTransitAndWithdraws(t *testing.T) {
	data := map[string][]byte{NodePublicKeyPrefix + "relay": []byte("relay-key"), NodeTunnelAddressPrefix + "relay": []byte("10.100.0.1"), NodeAddressesPrefix + "relay": []byte("10.10.0.11"), PeerPublicKeyPrefix + "gateway": []byte("gateway-key"), PeerRouteHostsPrefix + "gateway": []byte("10.100.0.136/32"), PeerAllowedIPsPrefix + "gateway": []byte("10.100.0.136/32,10.244.1.0/24")}
	projection := GatewayProjection{Lease: "worker/epoch", WorkerKey: "worker-key", GatewayKey: "gateway-key", WorkerAddress: netip.MustParseAddr("172.29.0.21"), WorkerHost: netip.MustParsePrefix("172.29.0.21/32")}
	raw, _ := json.Marshal([]GatewayProjection{projection})
	data[GatewayProjectionsKey] = raw
	transit, err := SiteTransit(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if transit == nil || transit.Via != "10.10.0.11" || !containsHost(transit.Hosts, "172.29.0.21") {
		t.Fatal("projected native worker absent from transit")
	}
	for _, block := range transit.Blocks {
		if block == "172.29.0.21/32" || block == "10.100.0.136/32" {
			t.Fatal("host misclassified as pod block")
		}
	}
	delete(data, GatewayProjectionsKey)
	restored, err := SiteTransit(data, nil)
	if err != nil || containsHost(restored.Hosts, "172.29.0.21") {
		t.Fatal("withdrawn host still routed")
	}
}
