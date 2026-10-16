package main

import (
	"encoding/json"
	"net"
	"os"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func requireStableAddressNamespace(t *testing.T) {
	t.Helper()
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
}

func TestStableAddressIdentityBindsAllocation(t *testing.T) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	request := stableAddressOwnerRequest{MeshUID: "mesh", NodeUID: "node", LegacyInterface: "legacy", RootPublicKey: key.PublicKey().String(), Addresses: []string{"10.100.0.1/24", "fd10::1/96"}}
	first, err := stableAddressIdentity(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Addresses = []string{"fd10::1/128", "10.100.0.1/32"}
	second, err := stableAddressIdentity(request)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("equivalent address ownership changed identity")
	}
	for _, field := range []string{"mesh", "node", "address", "key"} {
		changed := request
		switch field {
		case "mesh":
			changed.MeshUID = "other"
		case "node":
			changed.NodeUID = "other"
		case "address":
			changed.Addresses = []string{"10.100.0.2/32", "fd10::1/128"}
		case "key":
			key, _ := wgtypes.GeneratePrivateKey()
			changed.RootPublicKey = key.PublicKey().String()
		}
		other, err := stableAddressIdentity(changed)
		if err != nil || other.Interface == first.Interface || other.Alias == first.Alias {
			t.Fatalf("identity did not bind %s", field)
		}
	}
	for _, addresses := range [][]string{nil, {"127.0.0.1/32"}, {"fe80::1/128"}, {"10.100.0.1/32", "10.100.0.2/32"}, {"::ffff:10.100.0.1/128"}, {"invalid"}} {
		request.Addresses = addresses
		if _, err := stableAddressIdentity(request); err == nil {
			t.Fatalf("accepted invalid stable allocation %v", addresses)
		}
	}
}

func TestNativeStableAddressOwnerSurvivesLegacyRetirement(t *testing.T) {
	if os.Getenv("CLDT_STABLE_ADDRESS") != "1" {
		t.Skip("requires isolated VM namespace")
	}
	requireStableAddressNamespace(t)
	client, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, fault := range []string{"none", "foreign-address", "legacy-key", "owner-alias"} {
		t.Run(fault, func(t *testing.T) {
			key, err := wgtypes.GeneratePrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			request := stableAddressOwnerRequest{MeshUID: "mesh", NodeUID: "node", LegacyInterface: "stable-old", RootPublicKey: key.PublicKey().String(), Addresses: []string{"10.100.0.1/32", "fd10::1/128"}}
			if err := ensureLink(config{iface: request.LegacyInterface, mtu: 1400}, request.Addresses); err != nil {
				t.Fatal(err)
			}
			old, err := netlink.LinkByName(request.LegacyInterface)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = netlink.LinkDel(old) })
			if err := client.ConfigureDevice(request.LegacyInterface, wgtypes.Config{PrivateKey: &key}); err != nil {
				t.Fatal(err)
			}
			expected, err := stableAddressIdentity(request)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if link, err := netlink.LinkByName(expected.Interface); err == nil {
					_ = netlink.LinkDel(link)
				}
			})
			if fault == "legacy-key" {
				other, _ := wgtypes.GeneratePrivateKey()
				if err := client.ConfigureDevice(request.LegacyInterface, wgtypes.Config{PrivateKey: &other}); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "foreign-address" {
				link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "protected0"}}
				if err := netlink.LinkAdd(link); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = netlink.LinkDel(link) })
				addr, _ := netlink.ParseAddr(request.Addresses[0])
				if err := netlink.AddrAdd(link, addr); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "owner-alias" {
				link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: expected.Interface, Alias: "unrelated-owner"}}
				if err := netlink.LinkAdd(link); err != nil {
					t.Fatal(err)
				}
				if err := netlink.LinkSetAlias(link, "unrelated-owner"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := netlink.AddrList(old, netlink.FAMILY_ALL)
			if err != nil {
				t.Fatal(err)
			}
			binding, prepareErr := prepareStableAddressOwner(client, request)
			after, err := netlink.AddrList(old, netlink.FAMILY_ALL)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("preparation changed legacy addresses")
			}
			if fault != "none" {
				if prepareErr == nil {
					t.Fatal("unsafe owner request accepted")
				}
				owner, err := netlink.LinkByName(expected.Interface)
				if fault == "owner-alias" {
					if err != nil || owner.Attrs().Alias != "unrelated-owner" {
						t.Fatal("foreign owner changed")
					}
				} else if !isLinkNotFound(err) {
					t.Fatal("rejected request created an owner")
				}
				return
			}
			if prepareErr != nil {
				t.Fatal(prepareErr)
			}
			if !reflect.DeepEqual(binding, expected) {
				t.Fatal("binding differs")
			}
			removeDevice(request.LegacyInterface)
			if _, err := netlink.LinkByName(request.LegacyInterface); !isLinkNotFound(err) {
				t.Fatal("legacy tunnel was not retired")
			}
			// A restarted executor reconstructs the same owner from committed identity.
			repeated, err := prepareStableAddressOwner(client, request)
			if err != nil || !reflect.DeepEqual(binding, repeated) {
				t.Fatalf("owner did not survive retirement: %v", err)
			}
			owner, err := netlink.LinkByName(binding.Interface)
			if err != nil {
				t.Fatal(err)
			}
			missing, err := netlink.ParseAddr(binding.Addresses[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := netlink.AddrDel(owner, missing); err != nil {
				t.Fatal(err)
			}
			if _, err := prepareStableAddressOwner(client, request); err != nil {
				t.Fatalf("lost address not recovered on admitted owner: %v", err)
			}
			for _, raw := range binding.Addresses {
				ip, _, err := net.ParseCIDR(raw)
				if err != nil {
					t.Fatal(err)
				}
				listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip})
				if err != nil {
					t.Fatalf("stable address cannot source traffic after retirement: %v", err)
				}
				listener.Close()
			}
		})
	}
}

// Adapter for the existing native packet experiment. The surrounding process
// owns isolation, traffic observation, and generation-device retirement.
func TestNativePrepareStableAddressOwner(t *testing.T) {
	path := os.Getenv("CLDT_STABLE_ADDRESS_REQUEST")
	if path == "" {
		t.Skip("requires isolated packet experiment")
	}
	requireStableAddressNamespace(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		Request    stableAddressOwnerRequest `json:"request"`
		ResultFile string                    `json:"resultFile"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	client, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	binding, err := prepareStableAddressOwner(client, input.Request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := prepareStableAddressOwner(client, input.Request)
	if err != nil || !reflect.DeepEqual(binding, again) {
		t.Fatalf("stable owner not idempotent: %v", err)
	}
	raw, err = json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input.ResultFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
