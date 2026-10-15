package main

import (
	"fmt"
	"net"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// A fresh handshake proves the prepared transport can exchange authenticated
// packets. It is not a forwarding acknowledgement or a workload/MTU test.
const receiveHandshakeMaxAge = tunnel.ReceiveHandshakeMaxAge

func siteReceiveHandshake(devices receiverDevices, key wgtypes.Key, plan tunnel.SiteReceiveClient, mark int, now time.Time) (time.Time, error) {
	live, err := devices.Device(plan.Interface)
	if err != nil {
		return time.Time{}, err
	}
	if key == (wgtypes.Key{}) || key.PublicKey().String() != plan.SitePublicKey || live.PrivateKey != key || live.FirewallMark != mark || mark <= 0 || len(live.Peers) != 1 || live.Peers[0].PublicKey.String() != plan.PublicKey {
		return time.Time{}, fmt.Errorf("prepared site receiver identity changed")
	}
	endpoint, err := net.ResolveUDPAddr("udp", plan.Endpoint)
	if err != nil {
		return time.Time{}, err
	}
	peer := live.Peers[0]
	if peer.Endpoint == nil || !peer.Endpoint.IP.Equal(endpoint.IP) || peer.Endpoint.Port != endpoint.Port || peer.Endpoint.Zone != endpoint.Zone {
		return time.Time{}, fmt.Errorf("prepared site receiver endpoint changed")
	}
	allowed := prefixSet(peer.AllowedIPs)
	if len(allowed) != len(plan.AllowedSources) {
		return time.Time{}, fmt.Errorf("prepared site receiver permissions changed")
	}
	for _, prefix := range plan.AllowedSources {
		if !allowed[prefix] {
			return time.Time{}, fmt.Errorf("prepared site receiver permissions changed")
		}
	}
	handshake := peer.LastHandshakeTime
	if handshake.IsZero() || now.Before(handshake) || now.Sub(handshake) > receiveHandshakeMaxAge {
		return time.Time{}, nil
	}
	return handshake, nil
}
