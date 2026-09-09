// Linux reads the existing device through WireGuard's netlink API.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func openAdapter(name string) (adapterReader, error) {
	client, err := wgctrl.New()
	if err != nil {
		return adapterReader{}, err
	}
	return adapterReader{close: func() { client.Close() }, read: func() (bool, []peerObservation, error) {
		device, err := client.Device(name)
		if err != nil {
			return false, nil, err
		}
		iface, err := net.InterfaceByName(name)
		if err != nil {
			return false, nil, err
		}
		return iface.Flags&net.FlagUp != 0, observePeers(device), nil
	}}, nil
}

// Do not serialize the native device or peer structs: they contain private and
// preshared keys in addition to the public counters this observer needs.
func observePeers(device *wgtypes.Device) []peerObservation {
	peers := make([]peerObservation, 0, len(device.Peers))
	for _, peer := range device.Peers {
		identity := sha256.Sum256(peer.PublicKey[:])
		endpoint := ""
		if peer.Endpoint != nil {
			endpoint = peer.Endpoint.String()
		}
		peers = append(peers, peerObservation{Identity: hex.EncodeToString(identity[:]), Endpoint: endpoint, Tx: uint64(peer.TransmitBytes), Rx: uint64(peer.ReceiveBytes), Handshake: handshakeFiletime(peer.LastHandshakeTime)})
	}
	return peers
}

// Preserve the existing Windows observation schema on both platforms. Zero
// means no valid handshake; FILETIME counts 100ns intervals since 1601-01-01.
func handshakeFiletime(t time.Time) uint64 {
	const epochSeconds int64 = 11644473600
	if t.IsZero() || t.Unix() < -epochSeconds {
		return 0
	}
	return uint64(t.Unix()+epochSeconds)*10000000 + uint64(t.Nanosecond()/100)
}
