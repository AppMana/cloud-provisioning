package main

import (
	"os"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestNativeStableAddressRecovery(t *testing.T) {
	if os.Getenv("CLDT_STABLE_ADDRESS") != "1" {
		t.Skip("requires isolated VM mount and network namespaces")
	}
	requireStableAddressNamespace(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	devices, err := wgctrl.New()
	must(err)
	defer devices.Close()
	for _, fault := range []string{"none", "foreign-address", "foreign-owner", "foreign-legacy"} {
		t.Run(fault, func(t *testing.T) {
			key, err := wgtypes.GeneratePrivateKey()
			must(err)
			request := stableAddressOwnerRequest{MeshUID: "recovery", NodeUID: "node", LegacyInterface: "old", RootPublicKey: key.PublicKey().String(), Addresses: []string{"10.100.0.1/24", "fd10::1/64"}}
			binding, err := stableAddressIdentity(request)
			must(err)
			t.Cleanup(func() {
				if link, err := netlink.LinkByName(binding.Interface); err == nil {
					_ = netlink.LinkDel(link)
				}
			})
			if _, err := prepareStableAddressOwner(devices, request); err == nil {
				t.Fatal("ordinary preparation accepted an allocation without native evidence")
			}
			var foreign netlink.Link
			if fault != "none" {
				name := "protected0"
				if fault == "foreign-owner" {
					name = binding.Interface
				}
				if fault == "foreign-legacy" {
					name = request.LegacyInterface
				}
				foreign = &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
				must(netlink.LinkAdd(foreign))
				must(netlink.LinkSetAlias(foreign, "protected"))
				t.Cleanup(func() { _ = netlink.LinkDel(foreign) })
				if fault == "foreign-address" {
					address, err := netlink.ParseAddr("10.100.0.1/32")
					must(err)
					must(netlink.AddrAdd(foreign, address))
				}
			}
			before, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
			must(err)
			got, err := reconcileStableAddressOwner(devices, request, true)
			if fault != "none" {
				if err == nil {
					t.Fatal("recovery accepted foreign native state")
				}
				live, err := netlink.LinkByName(foreign.Attrs().Name)
				must(err)
				if live.Attrs().Alias != "protected" || live.Type() != "dummy" {
					t.Fatal("recovery changed foreign ownership")
				}
				after, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
				must(err)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("rejected recovery changed routes")
				}
				return
			}
			must(err)
			if !reflect.DeepEqual(got, binding) {
				t.Fatal("recovery changed the committed allocation")
			}
			_, err = verifyStableAddressOwner(request)
			must(err)
			// Loss of the complete owner, as on reboot, must be recoverable.
			owner, err := netlink.LinkByName(binding.Interface)
			must(err)
			must(netlink.LinkDel(owner))
			_, err = reconcileStableAddressOwner(devices, request, true)
			must(err)
			_, err = verifyStableAddressOwner(request)
			must(err)
			if _, err := netlink.LinkByName(request.LegacyInterface); !isLinkNotFound(err) {
				t.Fatal("allocation recovery recreated legacy transport", err)
			}
		})
	}
}
