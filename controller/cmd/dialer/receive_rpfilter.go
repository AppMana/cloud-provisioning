package main

import (
	"fmt"
	"strconv"

	"github.com/vishvananda/netlink"
)

// Unnumbered receive devices can accept traffic before the reverse route moves
// to them. Linux rejects that asymmetric input even in loose RPF mode when the
// incoming device has no IPv4 address. WireGuard's committed AllowedIPs remain
// the source authorization boundary for these owned devices.
func ensureReceiveReversePathFiltering(name, alias string) error {
	checkOwner := func() error {
		link, err := netlink.LinkByName(name)
		if err != nil {
			return err
		}
		if alias == "" || link.Type() != "wireguard" || link.Attrs().Alias != alias {
			return fmt.Errorf("receiver RPF requires an owned WireGuard device")
		}
		return nil
	}
	if err := checkOwner(); err != nil {
		return err
	}
	const prefix = "ipv4/conf/"
	read := func(name string) (int, error) {
		raw, err := readSysctl(prefix + name + "/rp_filter")
		if err != nil {
			return 0, err
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || value > 2 {
			return 0, fmt.Errorf("invalid reverse path policy for %s", name)
		}
		return value, nil
	}
	global, err := read("all")
	if err != nil {
		return err
	}
	preserve := func(name string) error {
		current, err := read(name)
		if err != nil {
			return err
		}
		// The kernel's effective policy is max(all, interface). Preserve that
		// policy before removing the global floor; do not disable other NICs.
		return setSysctl(prefix+name+"/rp_filter", strconv.Itoa(max(global, current)))
	}
	if global != 0 {
		// Newly created interfaces must inherit the preserved floor before the
		// live interface inventory is taken.
		if err := preserve("default"); err != nil {
			return err
		}
		links, err := netlink.LinkList()
		if err != nil {
			return err
		}
		for _, link := range links {
			if link.Attrs().Name == name {
				continue
			}
			if err := preserve(link.Attrs().Name); err != nil {
				return err
			}
		}
	}
	if err := checkOwner(); err != nil {
		return err
	}
	if err := setSysctl(prefix+name+"/rp_filter", "0"); err != nil {
		return err
	}
	if err := setSysctl(prefix+"all/rp_filter", "0"); err != nil {
		return err
	}
	if err := verifyReceiveReversePathFiltering(name); err != nil {
		return err
	}
	return checkOwner()
}

func verifyReceiveReversePathFiltering(name string) error {
	for _, scope := range []string{"all", name} {
		value, err := readSysctl("ipv4/conf/" + scope + "/rp_filter")
		if err != nil {
			return err
		}
		if value != "0" {
			return fmt.Errorf("unnumbered receiver reverse path filtering is enabled")
		}
	}
	return nil
}
