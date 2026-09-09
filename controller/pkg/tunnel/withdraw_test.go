package tunnel

import (
	"reflect"
	"testing"
)

func TestWithdrawRemotePeerPreservesSurvivorsAndReservations(t *testing.T) {
	data := map[string][]byte{
		PeerPublicKeyPrefix + "worker": []byte("old-key"), PeerEndpointPrefix + "worker": []byte("192.0.2.2:51820"),
		PeerAllowedIPsPrefix + "worker": []byte("10.100.0.2/32"), PeerRouteHostsPrefix + "worker": []byte("192.0.2.2"), PeerRouteHostPrefix + "worker": []byte("192.0.2.2"),
		PeerPublicKeyPrefix + "worker-other": []byte("survivor-key"), TunnelAddressReservationPrefix + "worker": []byte("10.100.0.2"),
	}
	if _, _, err := WithdrawRemotePeer(data, "worker", "replacement-key"); err == nil {
		t.Fatal("removed peer with another key")
	}
	if _, _, err := WithdrawRemotePeer(map[string][]byte{PeerEndpointPrefix + "worker": []byte("replacement")}, "worker", "old-key"); err == nil {
		t.Fatal("removed unidentified partial peer")
	}
	out, changed, err := WithdrawRemotePeer(data, "worker", "old-key")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if len(out) != 2 || string(out[PeerPublicKeyPrefix+"worker-other"]) != "survivor-key" || string(out[TunnelAddressReservationPrefix+"worker"]) != "10.100.0.2" {
		t.Fatal("changed survivors or address reservations", out)
	}
	if string(data[PeerPublicKeyPrefix+"worker"]) != "old-key" {
		t.Fatal("mutated source snapshot")
	}
	again, changed, err := WithdrawRemotePeer(out, "worker", "old-key")
	if err != nil || changed || !reflect.DeepEqual(out, again) {
		t.Fatal("withdrawal retry changed state", changed, err)
	}
	again[PeerPublicKeyPrefix+"worker-other"][0] = 'X'
	if string(out[PeerPublicKeyPrefix+"worker-other"]) != "survivor-key" {
		t.Fatal("snapshot aliases prior state")
	}
}
