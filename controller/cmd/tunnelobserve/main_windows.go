// Windows reads the existing adapter through the WireGuard driver API.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strconv"

	"golang.zx2c4.com/wireguard/windows/driver"
)

func openAdapter(name string) (adapterReader, error) {
	adapter, err := driver.OpenAdapter(name)
	if err != nil {
		return adapterReader{}, err
	}
	return adapterReader{close: func() { adapter.Close() }, read: func() (bool, []peerObservation, error) {
		cfg, err := adapter.Configuration()
		if err != nil {
			return false, nil, err
		}
		state, err := adapter.AdapterState()
		if err != nil {
			return false, nil, err
		}
		peers := make([]peerObservation, 0, cfg.PeerCount)
		p := cfg.FirstPeer()
		for i := uint32(0); i < cfg.PeerCount; i++ {
			identity := sha256.Sum256(p.PublicKey[:])
			peers = append(peers, peerObservation{
				Identity: hex.EncodeToString(identity[:]),
				Endpoint: net.JoinHostPort(p.Endpoint.Addr().String(), strconv.Itoa(int(p.Endpoint.Port()))),
				Tx:       p.TxBytes, Rx: p.RxBytes, Handshake: p.LastHandshake,
			})
			p = p.NextPeer()
		}
		return state == driver.AdapterStateUp, peers, nil
	}}, nil
}
