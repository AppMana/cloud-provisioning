package main

import (
	"net"
	"reflect"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestDeviceDeltaPreservesUnchangedLiveState(t *testing.T) {
	a, b := testPrefix("10.0.0.0/24"), testPrefix("fd00::/64")
	key, peer := wgtypes.Key{1}, wgtypes.Key{2}
	port, mark, keepalive := 51820, 517, 15*time.Second
	endpoint := &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 51820}
	live := &wgtypes.Device{PrivateKey: key, ListenPort: port, FirewallMark: mark, Peers: []wgtypes.Peer{{PublicKey: peer, Endpoint: endpoint, PersistentKeepaliveInterval: keepalive, AllowedIPs: []net.IPNet{a, b}}}}
	want := wgtypes.Config{PrivateKey: &key, ListenPort: &port, FirewallMark: &mark, Peers: []wgtypes.PeerConfig{{PublicKey: peer, Endpoint: endpoint, PersistentKeepaliveInterval: &keepalive, AllowedIPs: []net.IPNet{b, a}, ReplaceAllowedIPs: true}}}
	delta, changed := deviceDelta(want, live)
	if changed || delta.PrivateKey != nil || delta.ListenPort != nil || delta.FirewallMark != nil || len(delta.Peers) != 0 {
		t.Fatalf("unchanged device would be rewritten: %+v", delta)
	}
	if !want.Peers[0].ReplaceAllowedIPs || len(want.Peers[0].AllowedIPs) != 2 || len(live.Peers[0].AllowedIPs) != 2 {
		t.Fatal("delta planning mutated its inputs")
	}
	// A listening peer's learned endpoint must survive a desired nil endpoint.
	want.Peers[0].Endpoint = nil
	if _, changed := deviceDelta(want, live); changed {
		t.Fatal("roamed endpoint triggered a device update")
	}
	want.Peers[0].AllowedIPs[1].IP = net.ParseIP("10.0.0.7")
	if _, changed := deviceDelta(want, live); changed {
		t.Fatal("equivalent masked prefix triggered a device update")
	}
}

func TestDeviceDeltaUpdatesOnlyChangedPrefixes(t *testing.T) {
	a, b := testPrefix("10.0.0.0/24"), testPrefix("fd00::/64")
	key := wgtypes.Key{2}
	for _, tc := range []struct {
		name         string
		old, desired []net.IPNet
		expected     []net.IPNet
		replace      bool
	}{
		{"addition", []net.IPNet{a}, []net.IPNet{a, b, b}, []net.IPNet{b}, false},
		{"withdrawal", []net.IPNet{a, b}, []net.IPNet{a}, []net.IPNet{a}, true},
		{"withdraw-all", []net.IPNet{a}, []net.IPNet{}, []net.IPNet{}, true},
		{"repair-missing-prefix", nil, []net.IPNet{a}, []net.IPNet{a}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := &wgtypes.Device{Peers: []wgtypes.Peer{{PublicKey: key, AllowedIPs: tc.old}}}
			delta, changed := deviceDelta(wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: key, AllowedIPs: tc.desired, ReplaceAllowedIPs: true}}}, live)
			if !changed || len(delta.Peers) != 1 || delta.ReplacePeers || delta.Peers[0].Remove || delta.Peers[0].ReplaceAllowedIPs != tc.replace || !reflect.DeepEqual(delta.Peers[0].AllowedIPs, tc.expected) {
				t.Fatalf("incorrect prefix update: %+v", delta)
			}
		})
	}
}

func TestDeviceDeltaRepairsDriftAndRemovesOnlyAbsentPeers(t *testing.T) {
	key, removed, added := wgtypes.Key{1}, wgtypes.Key{2}, wgtypes.Key{3}
	prefix := testPrefix("10.0.0.1/32")
	port, mark, keepalive := 51821, 518, 25*time.Second
	endpoint := &net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 51820}
	live := &wgtypes.Device{Peers: []wgtypes.Peer{{PublicKey: key, AllowedIPs: []net.IPNet{prefix}}, {PublicKey: removed}}}
	want := wgtypes.Config{PrivateKey: &key, ListenPort: &port, FirewallMark: &mark, Peers: []wgtypes.PeerConfig{
		{PublicKey: key, Endpoint: endpoint, PersistentKeepaliveInterval: &keepalive, AllowedIPs: []net.IPNet{prefix}, ReplaceAllowedIPs: true},
		{PublicKey: added, AllowedIPs: []net.IPNet{testPrefix("fd00::1/128")}, ReplaceAllowedIPs: true},
	}}
	delta, changed := deviceDelta(want, live)
	if !changed || delta.PrivateKey == nil || delta.ListenPort == nil || delta.FirewallMark == nil || len(delta.Peers) != 3 || delta.ReplacePeers {
		t.Fatalf("drift not repaired: %+v", delta)
	}
	if delta.Peers[0].ReplaceAllowedIPs || len(delta.Peers[0].AllowedIPs) != 0 || delta.Peers[0].Endpoint != endpoint || delta.Peers[0].PersistentKeepaliveInterval == nil {
		t.Fatalf("endpoint/keepalive change rewrote prefixes: %+v", delta.Peers[0])
	}
	if delta.Peers[1].PublicKey != added || len(delta.Peers[1].AllowedIPs) != 1 || delta.Peers[1].Remove || delta.Peers[2].PublicKey != removed || !delta.Peers[2].Remove {
		t.Fatalf("incorrect peer lifecycle: %+v", delta.Peers)
	}
}

func testPrefix(cidr string) net.IPNet {
	_, prefix, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return *prefix
}
