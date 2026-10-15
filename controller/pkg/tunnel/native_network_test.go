package tunnel

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNativeNetworkProjectionPreservesSiteAndForeignPaths(t *testing.T) {
	for _, mode := range []string{"same", "foreign", "unverified", "legacy-self"} {
		t.Run(mode, func(t *testing.T) {
			data := map[string][]byte{
				NodePublicKeyPrefix + "site":     []byte("site-key"),
				NodeTunnelAddressPrefix + "site": []byte("10.100.0.1/24"),
				NodeAddressesPrefix + "site":     []byte("10.101.0.1"),
				NodePodCIDRsPrefix + "site":      []byte("10.101.128.0/26"),
				PeerPublicKeyPrefix + "self":     []byte("self-key"),
				PeerRouteHostsPrefix + "self":    []byte("10.100.0.128"),
				PeerAllowedIPsPrefix + "self":    []byte("10.100.0.128/32"),
				PeerPublicKeyPrefix + "other":    []byte("other-key"),
				PeerRouteHostsPrefix + "other":   []byte("10.100.0.129,172.29.0.27,2001:db8:1:0:1234::1"),
				PeerAllowedIPsPrefix + "other":   []byte("10.100.0.129/32,172.29.0.27/32,172.29.0.45/32,2001:db8:1::/80,2001:db8:1:0:1234::1/128"),
			}
			self := NativeNetwork{PublicKey: "self-key", ID: "aws:123:region:vpc", Prefixes: []string{"172.29.0.104/32"}}
			other := NativeNetwork{PublicKey: "other-key", ID: self.ID, Prefixes: []string{"172.29.0.27/32", "172.29.0.45/32", "2001:db8:1::/80", "2001:db8:1:0:1234::1/128"}}
			if mode == "foreign" {
				other.ID = "aws:123:region:other-vpc"
			}
			data[PeerNativeNetworkPrefix+"self"], _ = json.Marshal(self)
			data[PeerNativeNetworkPrefix+"other"], _ = json.Marshal(other)
			if mode == "unverified" {
				delete(data, PeerNativeNetworkPrefix+"self")
			}
			if mode == "legacy-self" {
				data[PeerRouteHostPrefix+"self"] = data[PeerRouteHostsPrefix+"self"]
				delete(data, PeerRouteHostsPrefix+"self")
			}
			siteBefore, err := SitePeerHash(data)
			if err != nil {
				t.Fatal(err)
			}
			peers, err := RemotePeers(data, "10.100.0.128", nil)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, peer := range peers {
				if peer.PublicKey != "other-key" {
					continue
				}
				found = true
				if mode == "same" || mode == "legacy-self" {
					if !reflect.DeepEqual(peer.WGAllowedIPs, []string{"10.100.0.129/32"}) || !reflect.DeepEqual(peer.RouteHosts, []string{"10.100.0.129"}) {
						t.Fatalf("native destinations still tunnelled: %+v", peer)
					}
				} else if len(peer.WGAllowedIPs) != 5 || len(peer.RouteHosts) != 3 {
					t.Fatalf("foreign/unverified peer lost reachability: %+v", peer)
				}
			}
			if !found {
				t.Fatal("transport peer removed")
			}
			siteAfter, _ := SitePeerHash(data)
			if siteAfter != siteBefore {
				t.Fatal("native projection mutated site routing")
			}
			site, err := SitePeers(data)
			if err != nil {
				t.Fatal(err)
			}
			for _, peer := range site {
				if peer.PublicKey == "other-key" && len(peer.WGAllowedIPs) != 5 {
					t.Fatal("site lost provider routes")
				}
			}
		})
	}
}

func TestNativeNetworkRejectsMalformedScope(t *testing.T) {
	for _, raw := range []string{`{"prefixes":["172.29.0.2/32"]}`, `{"publicKey":"self-key","id":"vpc","prefixes":["0.0.0.0/0"]}`, `{"publicKey":"self-key","id":"vpc","prefixes":["172.29.0.1/24"]}`, `{"publicKey":"self-key","id":"vpc","prefixes":["::ffff:172.29.0.2/128"]}`, `{`} {
		data := map[string][]byte{PeerPublicKeyPrefix + "self": []byte("self-key"), PeerNativeNetworkPrefix + "self": []byte(raw)}
		if _, err := projectNativeNetwork(data, "self", "other", PeerSpec{}); err == nil {
			t.Fatalf("accepted invalid native scope %s", raw)
		}
	}
}

func TestNativeNetworkRejectsReplacedPeerIdentity(t *testing.T) {
	network, _ := json.Marshal(NativeNetwork{ID: "aws:account:region:vpc", PublicKey: "old-key", Prefixes: []string{"172.29.0.4/32"}})
	data := map[string][]byte{PeerPublicKeyPrefix + "self": []byte("replacement-key"), PeerNativeNetworkPrefix + "self": network}
	if _, err := projectNativeNetwork(data, "self", "other", PeerSpec{}); err == nil {
		t.Fatal("accepted stale native-network ownership")
	}
}
