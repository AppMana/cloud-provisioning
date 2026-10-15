package main

import (
	"net"
	"os"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestReceiverIdentitySeparation(t *testing.T) {
	root, _ := wgtypes.GeneratePrivateKey()
	other, _ := wgtypes.GeneratePrivateKey()
	original := receiverKey(root, "mesh", "peer")
	if original != receiverKey(root, "mesh", "peer") {
		t.Fatal("restart changed receiver key")
	}
	for _, key := range []wgtypes.Key{root, receiverKey(other, "mesh", "peer"), receiverKey(root, "other", "peer"), receiverKey(root, "mesh", "other")} {
		if original.PublicKey() == key.PublicKey() {
			t.Fatal("receiver identities are not separated")
		}
	}
}

func TestReceiverPreparationRefusesWithdrawalsAndForeignState(t *testing.T) {
	root, _ := wgtypes.GeneratePrivateKey()
	peer, _ := wgtypes.GeneratePrivateKey()
	other, _ := wgtypes.GeneratePrivateKey()
	_, prefix, _ := net.ParseCIDR("10.90.0.10/32")
	path := tunnel.ReceivePath{PublicKey: peer.PublicKey().String(), AllowedSources: []string{"10.90.0.10/32"}}
	live := wgtypes.Device{PrivateKey: root, Peers: []wgtypes.Peer{{PublicKey: peer.PublicKey(), AllowedIPs: []net.IPNet{*prefix}}}}
	if err := validateReceiverState(&live, root, path); err != nil {
		t.Fatal(err)
	}
	if err := validateReceiverState(&live, other, path); err == nil {
		t.Fatal("foreign private key accepted")
	}
	path.AllowedSources = nil
	if err := validateReceiverState(&live, root, path); err == nil {
		t.Fatal("implicit withdrawal accepted")
	}
	path.AllowedSources = []string{"10.90.0.10/32"}
	live.Peers[0].PublicKey = other.PublicKey()
	if err := validateReceiverState(&live, root, path); err == nil {
		t.Fatal("foreign peer accepted")
	}
}

// Explicitly run inside an empty network namespace in an isolated lab VM.
func TestNativeReceivePreparation(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_RECEIVE_PATHS") != "1" {
		t.Skip("requires isolated VM network namespace")
	}
	self, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	init, err := os.Readlink("/proc/1/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if self == init {
		t.Fatal("refusing guest host namespace")
	}
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.Attrs().Name != "lo" {
			t.Fatal("namespace is not empty")
		}
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := wgtypes.GeneratePrivateKey()
	a, _ := wgtypes.GeneratePrivateKey()
	b, _ := wgtypes.GeneratePrivateKey()
	before := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: a.PublicKey().String(), WGAllowedIPs: []string{"10.90.0.10/32", "2001:db8:1::10/128"}}}}
	after := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: b.PublicKey().String(), WGAllowedIPs: []string{"10.90.0.10/32", "2001:db8:1::10/128"}}}}
	plan, err := tunnel.PlanReceiveHandover("native-mesh", before, after, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, path := range plan.Receivers {
			if link, err := netlink.LinkByName(path.Interface); err == nil {
				if err := netlink.LinkDel(link); err != nil {
					t.Error(err)
				}
			}
		}
	})
	devices, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer devices.Close()
	first, err := prepareReceivePaths(devices, "native-mesh", root, before, after, nil, 1400, 518)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].PublicKey == first[1].PublicKey || first[0].ListenPort == first[1].ListenPort {
		t.Fatal("receivers do not have distinct identities and ports")
	}
	second, err := prepareReceivePaths(devices, "native-mesh", root, before, after, nil, 1400, 518)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("repeated preparation changed receiver bindings")
	}
	for _, binding := range first {
		link, err := netlink.LinkByName(binding.Interface)
		if err != nil {
			t.Fatal(err)
		}
		addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			t.Fatal(err)
		}
		if len(addrs) != 0 {
			t.Fatal("preparation installed interface addresses")
		}
	}
	// Omitting a receiver from a later preparation must not retire its device.
	if _, err := prepareReceivePaths(devices, "native-mesh", root, after, after, nil, 1400, 518); err != nil {
		t.Fatal(err)
	}
	for _, binding := range first {
		if _, err := devices.Device(binding.Interface); err != nil {
			t.Fatal("preparation retired an old receiver")
		}
	}
	afterRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	afterRules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(routes, afterRoutes) || !reflect.DeepEqual(rules, afterRules) {
		t.Fatal("receiver preparation changed routing")
	}
	// A stale previous document cannot remove live permissions.
	narrowed := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: b.PublicKey().String(), WGAllowedIPs: []string{"10.90.0.10/32"}}}}
	if _, err := prepareReceivePaths(devices, "native-mesh", root, narrowed, narrowed, nil, 1400, 518); err == nil {
		t.Fatal("stale document withdrew live IPv6 permissions")
	}
	if _, err := prepareReceivePaths(devices, "native-mesh", a, before, after, nil, 1400, 518); err == nil {
		t.Fatal("changed root key overwrote live identities")
	}
	final, err := prepareReceivePaths(devices, "native-mesh", root, before, after, nil, 1400, 518)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, final) {
		t.Fatal("rejected preparation changed live bindings")
	}
}
