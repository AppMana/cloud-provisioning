package tunneldevice

import (
	"net"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Run in an isolated network namespace inside a VM. This is a kernel contract
// experiment, not a packet continuity test: acknowledgments cannot provide an
// overlap period if the receive device itself has only one prefix owner.
func TestKernelHandoverPrefixOwnership(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("requires CLDT_NETNS=1 in an isolated VM network namespace")
	}
	client, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	for _, name := range []string{"handover-old", "handover-new"} {
		link := &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: name}, LinkType: "wireguard"}
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { netlink.LinkDel(link) })
		key, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		if err := client.ConfigureDevice(name, wgtypes.Config{PrivateKey: &key}); err != nil {
			t.Fatal(err)
		}
	}
	oldKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	oldPeer, newPeer := oldKey.PublicKey(), newKey.PublicKey()
	var prefixes []net.IPNet
	for _, raw := range []string{"10.10.0.11/32", "10.1.190.64/26", "fd10::11/128", "fd20:1::/64"} {
		_, prefix, err := net.ParseCIDR(raw)
		if err != nil {
			t.Fatal(err)
		}
		prefixes = append(prefixes, *prefix)
	}
	apply := func(device string, peer wgtypes.Key) {
		t.Helper()
		if err := client.ConfigureDevice(device, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: peer, ReplaceAllowedIPs: true, AllowedIPs: prefixes}}}); err != nil {
			t.Fatal(err)
		}
	}
	assertOwner := func(device string, owner wgtypes.Key) {
		t.Helper()
		actual, err := client.Device(device)
		if err != nil {
			t.Fatal(err)
		}
		for _, prefix := range prefixes {
			count := 0
			for _, peer := range actual.Peers {
				for _, allowed := range peer.AllowedIPs {
					if allowed.String() == prefix.String() {
						count++
						if peer.PublicKey != owner {
							t.Fatalf("%s: prefix %s belongs to the wrong peer", device, prefix.String())
						}
					}
				}
			}
			if count != 1 {
				t.Fatalf("%s: prefix %s has %d owners", device, prefix.String(), count)
			}
		}
	}
	apply("handover-old", oldPeer)
	assertOwner("handover-old", oldPeer)
	// Adding the new peer does not retain the old peer's identical AllowedIPs.
	// Repeating either configuration is deterministic, including rollback.
	for _, owner := range []wgtypes.Key{newPeer, newPeer, oldPeer, newPeer} {
		apply("handover-old", owner)
		assertOwner("handover-old", owner)
	}
	// Separate receive devices can keep independent ownership. This alone does
	// not prove routing, authenticated source isolation, or Windows support.
	apply("handover-old", oldPeer)
	apply("handover-new", newPeer)
	assertOwner("handover-old", oldPeer)
	assertOwner("handover-new", newPeer)
	t.Log("IPv4/IPv6 host and pod prefixes: one owner per device; ownership independent across devices")
}
