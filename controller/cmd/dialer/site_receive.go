package main

import (
	"crypto/sha256"
	"fmt"
	"net"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// prepareSiteReceiveClient configures an independent device without changing
// routes or the legacy cloud peer. Its private key is the site's persisted key;
// the cloud public key differs from the legacy key, keeping Noise sessions
// separate even though the site uses one public identity on both devices.
func prepareSiteReceiveClient(devices receiverDevices, mesh string, siteKey wgtypes.Key, cloudRoot string, binding receiveBinding, previous, desired tunnel.PeerListDoc, mtu, mark int) (tunnel.SiteReceiveClient, error) {
	if siteKey == (wgtypes.Key{}) || mtu < 1280 || mtu > 65535 || mark <= 0 {
		return tunnel.SiteReceiveClient{}, fmt.Errorf("site receiver requires identity, MTU and firewall mark")
	}
	plan, err := tunnel.PlanSiteReceiveClient(mesh, siteKey.PublicKey().String(), cloudRoot, binding, previous, desired)
	if err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	endpoint, err := net.ResolveUDPAddr("udp", plan.Endpoint)
	if err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	sum := sha256.Sum256([]byte(mesh + "\x00" + plan.SitePublicKey + "\x00" + cloudRoot))
	alias := fmt.Sprintf("cloud-provisioning:receive-client:v1:%x", sum)
	link, err := netlink.LinkByName(plan.Interface)
	if err != nil {
		if !isLinkNotFound(err) {
			return tunnel.SiteReceiveClient{}, err
		}
		attrs := netlink.NewLinkAttrs()
		attrs.Name = plan.Interface
		attrs.MTU = mtu
		if err := netlink.LinkAdd(&netlink.GenericLink{LinkAttrs: attrs, LinkType: "wireguard"}); err != nil {
			return tunnel.SiteReceiveClient{}, err
		}
		link, err = netlink.LinkByName(plan.Interface)
		if err != nil {
			return tunnel.SiteReceiveClient{}, err
		}
		if err := netlink.LinkSetAlias(link, alias); err != nil {
			return tunnel.SiteReceiveClient{}, err
		}
		link, err = netlink.LinkByName(plan.Interface)
		if err != nil {
			return tunnel.SiteReceiveClient{}, err
		}
	}
	if link.Type() != "wireguard" || link.Attrs().Alias != alias {
		return tunnel.SiteReceiveClient{}, fmt.Errorf("site receiver device has foreign ownership")
	}
	live, err := devices.Device(plan.Interface)
	if err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	path := tunnel.ReceivePath{PublicKey: plan.PublicKey, AllowedSources: plan.AllowedSources}
	if err := validateReceiverState(live, siteKey, path); err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	for _, peer := range live.Peers {
		if peer.Endpoint != nil && (!peer.Endpoint.IP.Equal(endpoint.IP) || peer.Endpoint.Port != endpoint.Port || peer.Endpoint.Zone != endpoint.Zone) {
			return tunnel.SiteReceiveClient{}, fmt.Errorf("site receiver endpoint changed without a transport transition")
		}
	}
	allowed := make([]net.IPNet, 0, len(plan.AllowedSources))
	for _, raw := range plan.AllowedSources {
		_, prefix, _ := net.ParseCIDR(raw)
		allowed = append(allowed, *prefix)
	}
	public, _ := wgtypes.ParseKey(plan.PublicKey)
	keepalive := 25 * time.Second
	want := wgtypes.Config{PrivateKey: &siteKey, FirewallMark: &mark, Peers: []wgtypes.PeerConfig{{PublicKey: public, Endpoint: endpoint, AllowedIPs: allowed, ReplaceAllowedIPs: true, PersistentKeepaliveInterval: &keepalive}}}
	if delta, changed := deviceDelta(want, live); changed {
		if err := devices.ConfigureDevice(plan.Interface, delta); err != nil {
			return tunnel.SiteReceiveClient{}, err
		}
	}
	if err := netlink.LinkSetMTU(link, mtu); err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	live, err = devices.Device(plan.Interface)
	if err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	if live.PrivateKey != siteKey || live.FirewallMark != mark || len(live.Peers) != 1 || live.Peers[0].PublicKey != public || live.Peers[0].Endpoint == nil || !live.Peers[0].Endpoint.IP.Equal(endpoint.IP) || live.Peers[0].Endpoint.Port != endpoint.Port {
		return tunnel.SiteReceiveClient{}, fmt.Errorf("site receiver kernel identity mismatch")
	}
	actual := prefixSet(live.Peers[0].AllowedIPs)
	if len(actual) != len(plan.AllowedSources) {
		return tunnel.SiteReceiveClient{}, fmt.Errorf("site receiver kernel source permissions mismatch")
	}
	for _, prefix := range plan.AllowedSources {
		if !actual[prefix] {
			return tunnel.SiteReceiveClient{}, fmt.Errorf("site receiver kernel source permissions mismatch")
		}
	}
	return plan, nil
}
