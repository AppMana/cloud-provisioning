package tunnel

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"testing"
)

func TestRenderedRemoteDocumentKeepsCloudPeersOutOfSiteReceivers(t *testing.T) {
	for _, endpoint := range []string{PeerEndpointPending, "198.51.100.10:51820"} {
		for _, nativeRouting := range []bool{false, true} {
			t.Run(endpoint+map[bool]string{false: "/overlay", true: "/native"}[nativeRouting], func(t *testing.T) {
				site, self, other := receiverTestKey(8), receiverTestKey(16), receiverTestKey(24)
				data := map[string][]byte{
					NodePublicKeyPrefix + "site": []byte(site), NodeTunnelAddressPrefix + "site": []byte("10.100.0.1/24"), NodeAddressesPrefix + "site": []byte("10.101.0.1"), NodePodCIDRsPrefix + "site": []byte("10.101.128.0/26"),
					PeerPublicKeyPrefix + "self": []byte(self), PeerRouteHostsPrefix + "self": []byte("10.100.0.128"), PeerAllowedIPsPrefix + "self": []byte("10.100.0.128/32"),
					PeerPublicKeyPrefix + "other": []byte(other), PeerEndpointPrefix + "other": []byte(endpoint), PeerRouteHostsPrefix + "other": []byte("10.100.0.129,172.29.0.10"), PeerAllowedIPsPrefix + "other": []byte("10.100.0.129/32,172.29.0.10/32"),
				}
				if nativeRouting {
					data[PeerNativeNetworkPrefix+"self"], _ = json.Marshal(NativeNetwork{PublicKey: self, ID: "native-network", Prefixes: []string{"172.29.0.11/32"}})
					data[PeerNativeNetworkPrefix+"other"], _ = json.Marshal(NativeNetwork{PublicKey: other, ID: "native-network", Prefixes: []string{"172.29.0.10/32"}})
				}
				raw, err := RemotePeerDocument(data, "10.100.0.128", "10.101.0.9", "6443")
				if err != nil {
					t.Fatal(err)
				}
				var doc PeerListDoc
				if err := json.Unmarshal(raw, &doc); err != nil {
					t.Fatal(err)
				}
				if len(doc.Peers) != 2 {
					t.Fatal("remote document omitted a peer or included itself")
				}
				for _, peer := range doc.Peers {
					switch peer.PublicKey {
					case site:
						if peer.Remote {
							t.Fatal("site peer classified as cloud")
						}
					case other:
						if !peer.Remote {
							t.Fatal("cloud peer classified as site")
						}
						expected := []string{"10.100.0.129/32"}
						if !nativeRouting {
							expected = append(expected, "172.29.0.10/32")
						}
						if !reflect.DeepEqual(peer.WGAllowedIPs, expected) {
							t.Fatal("cloud routing changed")
						}
					default:
						t.Fatal("unexpected peer identity")
					}
				}
				plan, err := PlanReceiveHandover("mesh", doc, doc, []netip.Prefix{netip.MustParsePrefix("172.29.0.0/24")})
				if err != nil {
					t.Fatal(err)
				}
				if len(plan.Receivers) != 1 || plan.Receivers[0].PublicKey != site || !reflect.DeepEqual(plan.Desired, doc) {
					t.Fatal("real producer planned a cloud receiver or changed egress")
				}
			})
		}
	}
}
