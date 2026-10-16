package main

import (
	"github.com/vishvananda/netlink"
	"os"
	"reflect"
	"testing"
)

func TestNativeEnsureLinkRejectsForeignInterfaceWithoutMutation(t *testing.T) {
	if os.Getenv("CLDT_LINK_SAFETY") != "1" {
		t.Skip("requires isolated VM network namespace")
	}
	for _, kind := range []string{"net", "mnt"} {
		self, err := os.Readlink("/proc/self/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		host, err := os.Readlink("/proc/1/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		if self == host {
			t.Fatal("refusing guest host namespace")
		}
	}
	for _, kind := range []string{"dummy", "bridge", "veth", "wireguard"} {
		t.Run(kind, func(t *testing.T) {
			attrs := netlink.NewLinkAttrs()
			attrs.Name = "safety0"
			attrs.MTU = 1500
			var link netlink.Link
			switch kind {
			case "dummy":
				link = &netlink.Dummy{LinkAttrs: attrs}
			case "bridge":
				link = &netlink.Bridge{LinkAttrs: attrs}
			case "veth":
				link = &netlink.Veth{LinkAttrs: attrs, PeerName: "safety1"}
			case "wireguard":
				link = &netlink.GenericLink{LinkAttrs: attrs, LinkType: "wireguard"}
			}
			if err := netlink.LinkAdd(link); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = netlink.LinkDel(link) })
			addr, err := netlink.ParseAddr("192.0.2.1/24")
			if err != nil {
				t.Fatal(err)
			}
			if err = netlink.AddrAdd(link, addr); err != nil {
				t.Fatal(err)
			}
			if err = netlink.LinkSetUp(link); err != nil {
				t.Fatal(err)
			}
			before, err := netlink.AddrList(link, netlink.FAMILY_ALL)
			if err != nil {
				t.Fatal(err)
			}
			original, err := netlink.LinkByName(attrs.Name)
			if err != nil {
				t.Fatal(err)
			}
			addresses := []string{"10.100.0.1/32"}
			if kind == "wireguard" {
				addresses = append(addresses, "invalid-address")
			}
			applyErr := ensureLink(config{iface: attrs.Name, mtu: 1380}, addresses)
			after, err := netlink.AddrList(link, netlink.FAMILY_ALL)
			if err != nil {
				t.Fatal(err)
			}
			current, err := netlink.LinkByName(attrs.Name)
			if err != nil {
				t.Fatal(err)
			}
			if applyErr == nil || !reflect.DeepEqual(before, after) || original.Attrs().MTU != current.Attrs().MTU || original.Attrs().Flags != current.Attrs().Flags {
				t.Errorf("unsafe interface request was accepted or mutated state: rejection=%v addressesChanged=%v MTU=%d->%d", applyErr, !reflect.DeepEqual(before, after), original.Attrs().MTU, current.Attrs().MTU)
			}

			if kind != "wireguard" {
				removeDevice(attrs.Name)
				preserved, err := netlink.LinkByName(attrs.Name)
				if err != nil || preserved.Attrs().Index != original.Attrs().Index {
					t.Fatalf("retirement removed a foreign interface: %v", err)
				}
			}
		})
	}
	// Invalid addresses must not leave a partially created tunnel behind either.
	if err := ensureLink(config{iface: "safety-new", mtu: 1380}, []string{"10.100.0.1/32", "invalid-address"}); err == nil {
		t.Fatal("accepted invalid address")
	}
	if _, err := netlink.LinkByName("safety-new"); !isLinkNotFound(err) {
		t.Fatalf("invalid request left a device behind: %v", err)
	}
}
