package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Stable address ownership is independent of any receive generation. A caller
// must admit the committed allocation and serialize native phase execution.
// Preparing this owner neither selects egress nor authorizes device retirement.
type stableAddressOwnerRequest struct {
	MeshUID         string   `json:"meshUID"`
	NodeUID         string   `json:"nodeUID"`
	LegacyInterface string   `json:"legacyInterface"`
	RootPublicKey   string   `json:"rootPublicKey"`
	Addresses       []string `json:"addresses"`
}

type stableAddressOwnerBinding struct {
	Interface string   `json:"interface"`
	Alias     string   `json:"alias"`
	Addresses []string `json:"addresses"`
}

func stableAddressIdentity(request stableAddressOwnerRequest) (stableAddressOwnerBinding, error) {
	key, err := wgtypes.ParseKey(request.RootPublicKey)
	if strings.TrimSpace(request.MeshUID) == "" || strings.TrimSpace(request.NodeUID) == "" || err != nil || key == (wgtypes.Key{}) || request.LegacyInterface == "" || len(request.LegacyInterface) > 15 {
		return stableAddressOwnerBinding{}, fmt.Errorf("stable address owner requires mesh, node and legacy tunnel identities")
	}
	if len(request.Addresses) == 0 || len(request.Addresses) > 2 {
		return stableAddressOwnerBinding{}, fmt.Errorf("stable address owner requires one address per allocated family")
	}
	families := map[bool]bool{}
	binding := stableAddressOwnerBinding{}
	for _, raw := range request.Addresses {
		p, err := netip.ParsePrefix(raw)
		if err != nil || p.Addr().Is4In6() || !p.Addr().IsGlobalUnicast() || families[p.Addr().Is4()] {
			return stableAddressOwnerBinding{}, fmt.Errorf("invalid or duplicate stable address family")
		}
		families[p.Addr().Is4()] = true
		binding.Addresses = append(binding.Addresses, netip.PrefixFrom(p.Addr(), p.Addr().BitLen()).String())
	}
	slices.Sort(binding.Addresses)
	// Bind the allocation as well as identity: an address change must not silently
	// adopt an owner from a previous allocation. Prefix width is not ownership.
	raw, _ := json.Marshal([]any{request.MeshUID, request.NodeUID, request.RootPublicKey, binding.Addresses})
	sum := sha256.Sum256(raw)
	binding.Interface = fmt.Sprintf("clda%x", sum[:5])
	binding.Alias = fmt.Sprintf("cloud-provisioning:address-owner:v1:%x", sum)
	if binding.Interface == request.LegacyInterface {
		return stableAddressOwnerBinding{}, fmt.Errorf("address owner and generation device must differ")
	}
	return binding, nil
}

// prepareStableAddressOwner adds host addresses to a dedicated, identity-bound
// dummy device. It never removes addresses, devices, routes or policy rules.
// Existing addresses on the admitted legacy tunnel remain until a separately
// authorized retirement. A matching owner can recover missing addresses after
// restart, including when the old generation device is already absent.
func prepareStableAddressOwner(devices receiverDevices, request stableAddressOwnerRequest) (stableAddressOwnerBinding, error) {
	binding, err := stableAddressIdentity(request)
	if err != nil {
		return binding, err
	}
	wanted := map[string]bool{}
	for _, raw := range binding.Addresses {
		p, _ := netip.ParsePrefix(raw)
		wanted[p.Addr().String()] = true
	}
	owner, err := netlink.LinkByName(binding.Interface)
	if err != nil && !isLinkNotFound(err) {
		return binding, err
	}
	if err != nil {
		owner = nil
	}
	if owner != nil && (owner.Type() != "dummy" || owner.Attrs().Alias != binding.Alias) {
		return binding, fmt.Errorf("stable address interface is not owned by this allocation")
	}
	legacy, err := netlink.LinkByName(request.LegacyInterface)
	if err != nil && !isLinkNotFound(err) {
		return binding, err
	}
	if err != nil {
		legacy = nil
	}
	if legacy != nil {
		if legacy.Type() != "wireguard" {
			return binding, fmt.Errorf("legacy address source is not WireGuard")
		}
		if devices == nil {
			return binding, fmt.Errorf("legacy tunnel identity reader required")
		}
		live, err := devices.Device(request.LegacyInterface)
		if err != nil {
			return binding, err
		}
		if live.PublicKey.String() != request.RootPublicKey {
			return binding, fmt.Errorf("legacy tunnel public identity differs")
		}
	}
	if owner == nil && legacy == nil {
		return binding, fmt.Errorf("stable allocation has no current native owner")
	}
	links, err := netlink.LinkList()
	if err != nil {
		return binding, err
	}
	present := map[string]bool{}
	for _, link := range links {
		addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return binding, err
		}
		isOwner := owner != nil && link.Attrs().Index == owner.Attrs().Index
		isLegacy := legacy != nil && link.Attrs().Index == legacy.Attrs().Index
		for _, addr := range addresses {
			if addr.IP == nil || addr.IP.IsLinkLocalUnicast() {
				continue
			}
			matches := wanted[addr.IP.String()]
			if (isOwner || isLegacy) && !matches {
				return binding, fmt.Errorf("admitted address device contains an unexpected allocation")
			}
			if !matches {
				continue
			}
			if !isOwner && !isLegacy {
				return binding, fmt.Errorf("stable address is already assigned to another interface")
			}
			if isOwner {
				ones, bits := addr.Mask.Size()
				if ones != bits {
					return binding, fmt.Errorf("stable owner contains a connected network prefix")
				}
			}
			present[addr.IP.String()] = true
		}
	}
	if owner == nil {
		for address := range wanted {
			if !present[address] {
				return binding, fmt.Errorf("requested address is not assigned to the legacy tunnel")
			}
		}
		attrs := netlink.NewLinkAttrs()
		attrs.Name = binding.Interface
		attrs.Alias = binding.Alias
		if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: attrs}); err != nil {
			return binding, err
		}
		// Match receiver-device creation: kernels may omit the alias supplied
		// with LinkAdd, so bind the newly created device before adding addresses.
		if err := netlink.LinkSetAlias(&netlink.Dummy{LinkAttrs: attrs}, binding.Alias); err != nil {
			return binding, err
		}
		owner, err = netlink.LinkByName(binding.Interface)
		if err != nil {
			return binding, err
		}
		if owner.Type() != "dummy" || owner.Attrs().Alias != binding.Alias {
			return binding, fmt.Errorf("stable address owner identity was not retained by the kernel")
		}
	}
	if err := netlink.LinkSetUp(owner); err != nil {
		return binding, err
	}
	for _, raw := range binding.Addresses {
		addr, err := netlink.ParseAddr(raw)
		if err != nil {
			return binding, err
		}
		if addr.IP.To4() == nil {
			addr.Flags = unix.IFA_F_NODAD
		}
		if err := netlink.AddrAdd(owner, addr); err != nil && !isAddrExists(err) {
			return binding, err
		}
	}
	current, err := netlink.LinkByName(binding.Interface)
	if err != nil {
		return binding, err
	}
	if current.Attrs().Index != owner.Attrs().Index || current.Type() != "dummy" || current.Attrs().Alias != binding.Alias || current.Attrs().Flags&net.FlagUp == 0 {
		return binding, fmt.Errorf("stable owner changed during preparation")
	}
	addresses, err := netlink.AddrList(current, netlink.FAMILY_ALL)
	if err != nil {
		return binding, err
	}
	actual := []string{}
	for _, addr := range addresses {
		if !addr.IP.IsLinkLocalUnicast() {
			if addr.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) != 0 {
				return binding, fmt.Errorf("stable address is not usable")
			}
			actual = append(actual, addr.IPNet.String())
		}
	}
	slices.Sort(actual)
	if !slices.Equal(actual, binding.Addresses) {
		return binding, fmt.Errorf("stable address read-back differs from the committed allocation")
	}
	return binding, nil
}
