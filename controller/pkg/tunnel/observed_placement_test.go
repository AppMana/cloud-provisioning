package tunnel

import "testing"

// Observed on k0s v1.34.1+k0s.0 / bundled Calico 3.29.6-0: after
// workers -> cp, retained w1 (.1) kept winning over selected cp (.3).
// Both remotes acknowledged that list, but w1 could never retire because
// its own site address remained on its own peer instead of a replacement.
func TestObservedPlacement_RetainedLowerAddressYieldsTransit(t *testing.T) {
	data := map[string][]byte{
		NodePublicKeyPrefix + "w1":       []byte("W1KEY"),
		NodeTunnelAddressPrefix + "w1":   []byte("10.100.0.1/24"),
		NodeDepartedAtPrefix + "w1":      []byte("2026-09-05T19:33:42Z"),
		SiteAddressesPrefix + "w1":       []byte("10.10.0.11"),
		NodePublicKeyPrefix + "cp":       []byte("CPKEY"),
		NodeTunnelAddressPrefix + "cp":   []byte("10.100.0.3/24"),
		NodeAddressesPrefix + "cp":       []byte("10.10.0.10"),
		SiteAddressesPrefix + "w2":       []byte("10.10.0.12"),
		PeerPublicKeyPrefix + "remote1":  []byte("REMOTE1"),
		PeerRouteHostsPrefix + "remote1": []byte("10.100.0.130,203.0.113.10"),
	}
	peers, err := RemotePeers(data, "10.100.0.130", []string{"10.10.0.10"})
	if err != nil {
		t.Fatal(err)
	}
	if !DocRelaysNode(PeerListDoc{Peers: peers}, "W1KEY", []string{"10.10.0.11"}) {
		t.Error("departing worker cannot acknowledge transit through its replacement")
	}
	transit, err := SiteTransit(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if transit == nil || transit.Via != "10.10.0.10" {
		t.Errorf("site transit = %+v, want selected cp", transit)
	}
	// Retention still preserves the old peer until acknowledgment.
	if owners(peers, "10.100.0.1/32") != 1 {
		t.Error("retained tunnel disappeared")
	}
	// If no replacement key is published yet, preserve the old path.
	delete(data, NodePublicKeyPrefix+"cp")
	transit, err = SiteTransit(data, nil)
	if err != nil || transit == nil || transit.Via != "10.10.0.11" {
		t.Errorf("retained fallback = %+v, %v", transit, err)
	}
}
