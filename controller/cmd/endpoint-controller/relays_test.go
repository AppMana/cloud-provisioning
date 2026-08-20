package main

import (
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// A departing endpoint is held until every remote has moved, and the
// only honest reading of "moved" is what the remote's applied list
// says about THIS node. A list can be perfectly current and still
// route the departing node directly: right after a placement change
// that is exactly what every remote is holding, because the render
// that moves the node is the one they have not applied yet. Releasing
// on freshness takes the node's tunnel away while the remotes still
// accept its sources only there, and everything it sends is dropped
// by cryptokey routing until the next render lands.
//
// Measured as a sub-minute flap on the transit pairs after a
// placement shrink: cp3, no longer an endpoint, unable to reach a
// remote that had not yet learned to accept it through the relay.
func TestRelaysFromApplied(t *testing.T) {
	const (
		keyCP = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB="
		keyW1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA="
	)
	data := map[string][]byte{
		tunnel.NodePublicKeyPrefix + "cp":   []byte(keyCP),
		tunnel.NodePublicKeyPrefix + "w1":   []byte(keyW1),
		tunnel.SiteAddressesPrefix + "cp":   []byte("10.10.0.14"),
		tunnel.SiteAddressesPrefix + "w1":   []byte("10.10.0.11"),
		tunnel.PeerPublicKeyPrefix + "rem1": []byte("R1KEY"),
	}

	// What every remote is holding at the moment the placement
	// changes: current, applied, and still routing cp on its own
	// entry.
	direct := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{
		{PublicKey: keyCP, WGAllowedIPs: []string{"10.10.0.14/32"}},
		{PublicKey: keyW1, WGAllowedIPs: []string{"10.10.0.11/32"}},
	}}
	if got := relaysFromApplied(direct, data); got["cp"] {
		t.Error("a list that still routes cp directly was read as having moved it to a relay")
	}

	// The render that actually moves it: cp's addresses ride w1's
	// transit, and cp has no entry of its own.
	relayed := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{
		{PublicKey: keyW1, WGAllowedIPs: []string{"10.10.0.11/32", "10.10.0.14/32"},
			TransitHosts: []string{"10.10.0.14"}},
	}}
	got := relaysFromApplied(relayed, data)
	if !got["cp"] {
		t.Error("a list carrying cp on w1's transit was not read as relaying it")
	}
	// w1 is the relay, not relayed: releasing it on this evidence
	// would tear down the path everyone else is using.
	if got["w1"] {
		t.Error("the relay itself was read as relayed")
	}
}

// The hold is per departing node, so one node's move cannot release
// another's. Two nodes leaving at once is the ordinary case when a
// placement shrinks from all to a pair.
func TestEveryRemoteAcknowledgedIsPerNode(t *testing.T) {
	data := map[string][]byte{
		tunnel.PeerPublicKeyPrefix + "rem1": []byte("R1KEY"),
		tunnel.PeerPublicKeyPrefix + "rem2": []byte("R2KEY"),
	}
	converged := map[string]map[string]bool{
		"rem1": {"cp": true, "cp2": false},
		"rem2": {"cp": true, "cp2": true},
	}
	if !everyRemoteAcknowledged(data, converged, "cp") {
		t.Error("cp was held although every remote's applied list relays it")
	}
	if everyRemoteAcknowledged(data, converged, "cp2") {
		t.Error("cp2 was released although rem1's applied list still routes it directly")
	}
	// A machine that acknowledged nothing holds every node: absence of
	// evidence is not evidence of a move.
	if everyRemoteAcknowledged(data, map[string]map[string]bool{"rem1": {"cp": true}}, "cp") {
		t.Error("cp was released although rem2 acknowledged nothing at all")
	}
}
