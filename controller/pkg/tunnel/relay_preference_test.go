package tunnel

import "testing"

func TestRelayAdditionPreservesPublishedIdentity(t *testing.T) {
	data := map[string][]byte{
		NodePublicKeyPrefix + "w1":     []byte("key-one"),
		NodeTunnelAddressPrefix + "w1": []byte("10.100.0.2/24"),
		NodeAddressesPrefix + "w1":     []byte("192.0.2.1"),
		SiteAddressesPrefix + "cp":     []byte("192.0.2.10"),
	}
	if !PinRelay(data) || string(data[RelayPublicKeyKey]) != "key-one" {
		t.Fatal("initial relay not pinned")
	}
	data[NodePublicKeyPrefix+"w2"] = []byte("key-two")
	data[NodeTunnelAddressPrefix+"w2"] = []byte("10.100.0.1/24")
	data[NodeAddressesPrefix+"w2"] = []byte("192.0.2.2")
	if PinRelay(data) {
		t.Fatal("adding lower address changed relay")
	}
	transit, err := SiteTransit(data, nil)
	if err != nil || transit == nil || transit.Via != "192.0.2.1" {
		t.Fatalf("site moved to new relay: %+v %v", transit, err)
	}
	peers, err := RemotePeers(data, "10.100.0.130", nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range peers {
		for _, h := range p.TransitHosts {
			if HostCIDR(h) == "192.0.2.10/32" {
				found = true
				if p.PublicKey != "key-one" {
					t.Fatal("remote moved transit ownership")
				}
			}
		}
	}
	if !found {
		t.Fatal("missing control-plane transit")
	}
	transit, err = SiteTransit(data, map[string]bool{"w1": true})
	if err != nil || transit == nil || transit.Via != "192.0.2.2" {
		t.Fatalf("preference defeated readiness fallback: %+v %v", transit, err)
	}

}

func TestRelayPreferenceCannotRetainDepartedOrReplacedIdentity(t *testing.T) {
	for _, change := range []string{"departed", "removed", "rotated", "no-address"} {
		t.Run(change, func(t *testing.T) {
			data := map[string][]byte{RelayPublicKeyKey: []byte("old"), NodePublicKeyPrefix + "w1": []byte("old"), NodeTunnelAddressPrefix + "w1": []byte("10.100.0.2/24"), NodePublicKeyPrefix + "w2": []byte("other"), NodeTunnelAddressPrefix + "w2": []byte("10.100.0.1/24")}
			switch change {
			case "departed":
				data[NodeDepartedAtPrefix+"w1"] = []byte("now")
			case "removed":
				delete(data, NodePublicKeyPrefix+"w1")
			case "rotated":
				data[NodePublicKeyPrefix+"w1"] = []byte("new")
			case "no-address":
				delete(data, NodeTunnelAddressPrefix+"w1")
			}
			if !PinRelay(data) || string(data[RelayPublicKeyKey]) != "other" {
				t.Fatal("stale preference retained")
			}
		})
	}
	data := map[string][]byte{RelayPublicKeyKey: []byte("missing")}
	if !PinRelay(data) || len(data) != 0 {
		t.Fatal("empty mesh retained stale preference")
	}
}
