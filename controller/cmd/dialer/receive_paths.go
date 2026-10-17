package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net"
	"net/netip"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type receiveBinding = tunnel.ReceiveBinding

type receiverDevices interface {
	Device(string) (*wgtypes.Device, error)
	ConfigureDevice(string, wgtypes.Config) error
}

func receiverKey(root wgtypes.Key, mesh, peer string) wgtypes.Key {
	mac := hmac.New(sha256.New, root[:])
	mac.Write([]byte("cloud-provisioning/receive-key/v1\x00" + mesh + "\x00" + peer))
	var key wgtypes.Key
	copy(key[:], mac.Sum(nil))
	key[0] &= 248
	key[31] &= 127
	key[31] |= 64
	return key
}

func receiverAlias(mesh, peer string) string {
	sum := sha256.Sum256([]byte(mesh + "\x00" + peer))
	return fmt.Sprintf("cloud-provisioning:receive:v1:%x", sum)
}

// prepareReceivePaths creates independent accept tries without installing
// addresses, routes, or policy rules. Keys derive from the persisted node key;
// explicit controller reservations preserve listener ports across recreation.
// Preparation never selects an ephemeral port or silently moves a live port.
// This operation only adds permissions. Withdrawal requires a separate drain
// protocol; an incomplete previous document must not implicitly withdraw them.
func prepareReceivePaths(devices receiverDevices, mesh string, root wgtypes.Key, previous, desired tunnel.PeerListDoc, native []netip.Prefix, mtu, mark int, ports map[string]int) ([]receiveBinding, error) {
	if root == (wgtypes.Key{}) || mtu < 1280 || mtu > 65535 || mark <= 0 {
		return nil, fmt.Errorf("receiver preparation requires a private identity, valid MTU, and positive firewall mark")
	}
	plan, err := tunnel.PlanReceiveHandover(mesh, previous, desired, native)
	if err != nil {
		return nil, err
	}
	if err := tunnel.ValidateReceivePorts(plan.Receivers, ports); err != nil {
		return nil, err
	}
	// Check every existing device before preparing any of the new devices.
	for _, path := range plan.Receivers {
		link, err := netlink.LinkByName(path.Interface)
		if err != nil {
			if isLinkNotFound(err) {
				continue
			}
			return nil, err
		}
		if link.Type() != "wireguard" || link.Attrs().Alias != receiverAlias(mesh, path.PublicKey) {
			return nil, fmt.Errorf("receiver interface is not owned by this mesh and peer")
		}
		live, err := devices.Device(path.Interface)
		if err != nil {
			return nil, err
		}
		if live.ListenPort != 0 && live.ListenPort != ports[path.PublicKey] {
			return nil, fmt.Errorf("receiver listener port changed without retirement")
		}
		if err := validateReceiverState(live, receiverKey(root, mesh, path.PublicKey), path); err != nil {
			return nil, err
		}
	}
	bindings := make([]receiveBinding, 0, len(plan.Receivers))
	for _, path := range plan.Receivers {
		key := receiverKey(root, mesh, path.PublicKey)
		peer, _ := wgtypes.ParseKey(path.PublicKey) // Planner validated the identity.
		allowed := make([]net.IPNet, 0, len(path.AllowedSources))
		for _, raw := range path.AllowedSources {
			_, prefix, _ := net.ParseCIDR(raw)
			allowed = append(allowed, *prefix)
		}
		link, err := netlink.LinkByName(path.Interface)
		if err != nil {
			if !isLinkNotFound(err) {
				return nil, err
			}
			attrs := netlink.NewLinkAttrs()
			attrs.Name, attrs.Alias, attrs.MTU = path.Interface, receiverAlias(mesh, path.PublicKey), mtu
			if err := netlink.LinkAdd(&netlink.GenericLink{LinkAttrs: attrs, LinkType: "wireguard"}); err != nil {
				return nil, err
			}
			link, err = netlink.LinkByName(path.Interface)
			if err != nil {
				return nil, err
			}
			// Some kernels do not preserve IFLA_IFALIAS on WireGuard
			// creation. Bind ownership explicitly before adding any key.
			if err := netlink.LinkSetAlias(link, receiverAlias(mesh, path.PublicKey)); err != nil {
				return nil, err
			}
			link, err = netlink.LinkByName(path.Interface)
			if err != nil {
				return nil, err
			}
		}
		if link.Type() != "wireguard" || link.Attrs().Alias != receiverAlias(mesh, path.PublicKey) {
			return nil, fmt.Errorf("receiver ownership changed during preparation: type=%q alias=%q", link.Type(), link.Attrs().Alias)
		}
		live, err := devices.Device(path.Interface)
		if err != nil {
			return nil, err
		}
		if live.ListenPort != 0 && live.ListenPort != ports[path.PublicKey] {
			return nil, fmt.Errorf("receiver listener port changed during preparation")
		}
		if err := validateReceiverState(live, key, path); err != nil {
			return nil, err
		}
		port := ports[path.PublicKey]
		want := wgtypes.Config{PrivateKey: &key, ListenPort: &port, FirewallMark: &mark, Peers: []wgtypes.PeerConfig{{PublicKey: peer, AllowedIPs: allowed, ReplaceAllowedIPs: true}}}
		if delta, changed := deviceDelta(want, live); changed {
			if err := devices.ConfigureDevice(path.Interface, delta); err != nil {
				return nil, err
			}
		}
		if err := netlink.LinkSetMTU(link, mtu); err != nil {
			return nil, err
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return nil, err
		}
		if err := ensureReceiveReversePathFiltering(path.Interface, receiverAlias(mesh, path.PublicKey)); err != nil {
			return nil, err
		}
		live, err = devices.Device(path.Interface)
		if err != nil {
			return nil, err
		}
		if live.PrivateKey != key || live.ListenPort != port || live.FirewallMark != mark || len(live.Peers) != 1 || live.Peers[0].PublicKey != peer {
			return nil, fmt.Errorf("receiver kernel identity did not match preparation")
		}
		actual, wanted := prefixSet(live.Peers[0].AllowedIPs), prefixSet(allowed)
		if len(actual) != len(wanted) {
			return nil, fmt.Errorf("receiver kernel permissions did not match preparation")
		}
		for prefix := range wanted {
			if !actual[prefix] {
				return nil, fmt.Errorf("receiver kernel permissions did not match preparation")
			}
		}
		bindings = append(bindings, receiveBinding{path.Interface, path.PublicKey, key.PublicKey().String(), live.ListenPort})
	}
	return bindings, nil
}

func validateReceiverState(live *wgtypes.Device, key wgtypes.Key, path tunnel.ReceivePath) error {
	if live.PrivateKey != (wgtypes.Key{}) && live.PrivateKey != key {
		return fmt.Errorf("receiver private identity differs from persisted node identity")
	}
	allowed := map[string]bool{}
	for _, prefix := range path.AllowedSources {
		allowed[prefix] = true
	}
	for _, peer := range live.Peers {
		if peer.PublicKey.String() != path.PublicKey {
			return fmt.Errorf("receiver contains an unexpected peer")
		}
		for _, prefix := range peer.AllowedIPs {
			if !allowed[canonicalPrefix(prefix)] {
				return fmt.Errorf("receiver preparation would withdraw existing source permissions")
			}
		}
	}
	return nil
}
