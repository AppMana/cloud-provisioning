package tunneldevice

import (
	"golang.zx2c4.com/wireguard/windows/driver"
	"testing"
)

// The native removal capture showed surviving peers' counters dropping to zero
// together. The update must retain their driver objects when membership changes.
func TestPeerUpdatePreservesSurvivorsAndRemovesRetiredPeers(t *testing.T) {
	p, err := Compile(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	var retired [32]byte
	retired[0] = 42
	cfg, size, err := buildPeerUpdate(p, 51820, [][32]byte{p.Peers[0].Key, retired})
	if err != nil {
		t.Fatal(err)
	}
	if size == 0 || cfg.Flags&driver.InterfaceReplacePeers != 0 || cfg.PeerCount != 2 {
		t.Fatal("whole peer replacement or wrong count")
	}
	r := cfg.FirstPeer()
	if r.PublicKey != retired || r.Flags != driver.PeerHasPublicKey|driver.PeerRemove || r.AllowedIPsCount != 0 {
		t.Fatal("retired peer not explicitly removed")
	}
	survivor := r.NextPeer()
	if survivor.PublicKey != p.Peers[0].Key || survivor.Flags&driver.PeerRemove != 0 || survivor.Flags&driver.PeerReplaceAllowedIPs == 0 || survivor.AllowedIPsCount != uint32(len(p.Peers[0].Allowed)) {
		t.Fatal("survivor not updated in place")
	}
	if survivor.Endpoint.Addr() != p.Peers[0].Endpoint.Addr() || survivor.Endpoint.Port() != p.Peers[0].Endpoint.Port() {
		t.Fatal("desired endpoint omitted")
	}
}

func TestPeerUpdateAddsPeersAndCanRemoveAllPeers(t *testing.T) {
	p, err := Compile(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := buildPeerUpdate(p, 51820, nil)
	if err != nil || cfg.PeerCount != 1 || cfg.FirstPeer().Flags&driver.PeerUpdateOnly != 0 {
		t.Fatal("new peer cannot be created", err)
	}
	current := [][32]byte{p.Peers[0].Key}
	p.Peers = nil
	cfg, _, err = buildPeerUpdate(p, 51820, current)
	if err != nil || cfg.PeerCount != 1 || cfg.FirstPeer().Flags&driver.PeerRemove == 0 {
		t.Fatal("last peer retained", err)
	}
	cfg, _, err = buildPeerUpdate(p, 51820, nil)
	if err != nil || cfg.PeerCount != 0 {
		t.Fatal("empty adapter update invalid", err)
	}
}
