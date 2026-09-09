package tunnel

import (
	"encoding/json"
	"testing"
)

func TestLastWorkerRemovalRendersExplicitEmptySitePeers(t *testing.T) {
	data := map[string][]byte{
		PeerPublicKeyPrefix + "worker":  []byte("worker-public-key"),
		PeerAllowedIPsPrefix + "worker": []byte("10.253.253.2/32"),
		PeerRouteHostsPrefix + "worker": []byte("10.253.253.2"),
	}
	before, err := SitePeers(data)
	if err != nil || len(before) != 1 {
		t.Fatalf("initial render: %v %v", before, err)
	}
	for key := range data {
		delete(data, key)
	}
	after, err := SitePeers(data)
	if err != nil || after == nil || len(after) != 0 {
		t.Fatalf("last worker withdrawal: %v %v", after, err)
	}
	raw, err := json.Marshal(PeerListDoc{Peers: after})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["peers"]) != "[]" {
		t.Fatalf("empty worker set serialized as %s", doc["peers"])
	}
	hash, err := SitePeerHash(data)
	if err != nil || hash != HashPeerList(raw) {
		t.Fatal("site receipt hash differs from rendered empty state")
	}
	// The remote's last path protection is a separate contract.
	remote, err := RemotePeerDocument(data, "10.253.253.2", "10.10.0.10", "6443")
	if err != nil || len(remote) != 0 {
		t.Fatal("unready remote bootstrap rendered a withdrawal")
	}
}
