package main

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// restoreReceiveRoot restores only the shared device identity. Its caller must
// hold terminal recovery authority and the host lock. Peer, address and route writers
// retain their separate authorities. Owned unnumbered roots use the existing
// receive-device reverse-path policy without weakening other interfaces.
func restoreReceiveRoot(ctx context.Context, devices receiverDevices, cfg config, key wgtypes.Key, meshUID, nodeUID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if devices == nil || key == (wgtypes.Key{}) || meshUID == "" || nodeUID == "" || cfg.iface == "" || len(cfg.iface) > 15 || cfg.fwmark <= 0 || cfg.listenPort < 0 || cfg.listenPort > 65535 {
		return fmt.Errorf("root recovery requires admitted device identity")
	}
	sum := sha256.Sum256([]byte(meshUID + "\x00" + nodeUID + "\x00" + key.PublicKey().String()))
	alias := fmt.Sprintf("cloud-provisioning:root:v1:%x", sum)
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil && !isLinkNotFound(err) {
		return err
	}
	if err != nil {
		mtu := cfg.mtu
		if mtu == 0 {
			mtu = underlayMTU()
		}
		if mtu < 1280 || mtu > 65535 {
			return fmt.Errorf("root recovery requires a valid MTU")
		}
		if err := persistReceiveSysctlPolicy(cfg.iface, alias); err != nil {
			return err
		}
		link, err = createWireGuardLink(cfg, mtu)
		if err != nil {
			return err
		}
		if err := netlink.LinkSetAlias(link, alias); err != nil {
			return err
		}
		link, err = netlink.LinkByName(cfg.iface)
		if err != nil {
			return err
		}
	}
	if link.Type() != "wireguard" || (link.Attrs().Alias != "" && link.Attrs().Alias != alias) {
		return fmt.Errorf("root recovery found foreign device ownership")
	}
	live, err := devices.Device(cfg.iface)
	if err != nil {
		return err
	}
	if live.PublicKey == (wgtypes.Key{}) {
		if link.Attrs().Alias != alias || len(live.Peers) != 0 || live.FirewallMark != 0 || live.ListenPort != 0 {
			return fmt.Errorf("root recovery cannot adopt an unbound device")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Do not use deviceDelta: a nil desired peer set there means removal.
		if err := devices.ConfigureDevice(cfg.iface, wgtypes.Config{PrivateKey: &key, FirewallMark: &cfg.fwmark, ListenPort: &cfg.listenPort}); err != nil {
			return err
		}
	} else if live.PublicKey != key.PublicKey() || live.FirewallMark != cfg.fwmark || (cfg.listenPort != 0 && live.ListenPort != cfg.listenPort) {
		return fmt.Errorf("root recovery found a different native identity")
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}
	live, err = devices.Device(cfg.iface)
	if err != nil {
		return err
	}
	if live.PublicKey != key.PublicKey() || live.FirewallMark != cfg.fwmark || (cfg.listenPort != 0 && live.ListenPort != cfg.listenPort) {
		return fmt.Errorf("root recovery identity readback differs")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Older roots have no alias. Bind only after key, mark and port admission;
	// an empty-key or foreign-identity device was rejected above.
	if link.Attrs().Alias == "" {
		if err := netlink.LinkSetAlias(link, alias); err != nil {
			return err
		}
	}
	if err := ensureReceiveReversePathFiltering(cfg.iface, alias); err != nil {
		return err
	}
	return ctx.Err()
}
