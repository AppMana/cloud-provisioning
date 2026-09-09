package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestLinuxObservationSelectsOnlyPublicCounters(t *testing.T) {
	private, preshared, public := wgtypes.Key{}, wgtypes.Key{}, wgtypes.Key{}
	for i := range private {
		private[i] = byte(i + 1)
		preshared[i] = byte(i + 33)
		public[i] = byte(i + 65)
	}
	device := &wgtypes.Device{PrivateKey: private, Peers: []wgtypes.Peer{{PublicKey: public, PresharedKey: preshared, Endpoint: &net.UDPAddr{IP: net.ParseIP("203.0.113.10"), Port: 51820}, TransmitBytes: 123, ReceiveBytes: 456, LastHandshakeTime: time.Unix(0, 123456700).UTC()}}}
	peers := observePeers(device)
	sum := sha256.Sum256(public[:])
	if len(peers) != 1 || peers[0].Identity != hex.EncodeToString(sum[:]) || peers[0].Endpoint != "203.0.113.10:51820" || peers[0].Tx != 123 || peers[0].Rx != 456 || peers[0].Handshake != 116444736001234567 {
		t.Fatalf("wrong observation: %+v", peers)
	}
	raw, err := json.Marshal(peers)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{private.String(), preshared.String(), public.String(), "privateKey", "presharedKey"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("serialized configuration key material")
		}
	}
}

func TestNeverHandshakenPeerAndFiletimeEpoch(t *testing.T) {
	peers := observePeers(&wgtypes.Device{Peers: []wgtypes.Peer{{}}})
	if len(peers) != 1 || peers[0].Endpoint != "" || peers[0].Handshake != 0 {
		t.Fatalf("unexpected unset state: %+v", peers)
	}
	if handshakeFiletime(time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC)) != 0 || handshakeFiletime(time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)) != 0 {
		t.Fatal("invalid FILETIME epoch handling")
	}
}
