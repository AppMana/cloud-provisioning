package bringup

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"sigs.k8s.io/yaml"
)

// IdentityNetplanPath is where a machine's identity configuration lives, in
// the file its own provisioning would write.
const IdentityNetplanPath = "/etc/netplan/70-cluster-vip.yaml"

// identities gives identity-site nodes their dummy device and every LAN host
// its on-link routes to the identity prefixes. See lab/identity.go.
//
// A machine gets them in netplan, as a provisioned site node does, so a reboot
// row finds them where the node's own boot puts them; a container appliance
// has no boot to survive and gets them directly.
func identities(ctx context.Context, t lab.Topology, r rig.Rig) error {
	if len(t.IdentityRoutes) == 0 {
		return nil
	}
	for _, n := range t.NodesInRole(lab.ControlPlane, lab.Worker, lab.Bastion) {
		node := r.Node(n.Name)
		if n.IsClusterNode() {
			doc, err := identityNetplan(n, node.Interface(0), t.IdentityRoutes)
			if err != nil {
				return err
			}
			if err := node.Put(ctx, bytes.NewReader(doc), IdentityNetplanPath, 0o600); err != nil {
				return fmt.Errorf("%s identity configuration: %w", n.Name, err)
			}
			if path, ndp := identityProxyNDP(n, node.Interface(0)); path != "" {
				if err := node.Put(ctx, bytes.NewReader(ndp), path, 0o644); err != nil {
					return fmt.Errorf("%s identity neighbour discovery: %w", n.Name, err)
				}
			}
			if _, err := node.Exec(ctx, "netplan", "apply"); err != nil {
				return fmt.Errorf("%s applying identity configuration: %w", n.Name, err)
			}
			if err := configureIdentityNodeNetwork(ctx, n, node); err != nil {
				return err
			}
			continue
		}
		if err := applianceIdentityRoutes(ctx, t, n.Name, node); err != nil {
			return err
		}
	}
	return nil
}

// configureIdentityNodeNetwork applies the additional host-level networking traits
// of identity-site nodes (RoCE /30 jumbo point-to-point links with strict
// ARP, multi-metric on-link cluster-VIP routes with explicit source address, and
// Tailscale table 52 + Calico 0x80000 fwmark policy rules).
func configureIdentityNodeNetwork(ctx context.Context, n lab.Node, node rig.Node) error {
	for _, roce := range n.RoCELinks {
		if _, err := node.Exec(ctx, "sh", "-ec", fmt.Sprintf(
			"ip link show dev %s >/dev/null 2>&1 || ip link add %s type dummy; "+
				"ip link set dev %s mtu %d up; "+
				"ip addr replace %s dev %s; "+
				"sysctl -qw net.ipv4.conf.%s.arp_ignore=1 net.ipv4.conf.%s.arp_announce=2",
			roce.Name, roce.Name, roce.Name, roce.MTU, roce.Address, roce.Name, roce.Name, roce.Name,
		)); err != nil {
			return fmt.Errorf("%s RoCE link %s: %w", n.Name, roce.Name, err)
		}
	}
	if src := n.ClusterAddress(); src != "" {
		dev := node.Interface(0)
		for _, metric := range []string{"100", "300", "640"} {
			if _, err := node.Exec(ctx, "ip", "-4", "route", "replace", lab.IdentityCIDR4,
				"dev", dev, "proto", "static", "scope", "link", "src", src, "metric", metric); err != nil {
				return fmt.Errorf("%s fallback route metric %s: %w", n.Name, metric, err)
			}
		}
	}
	for _, argv := range tailscaleCalicoRuleCommands() {
		if _, err := node.Exec(ctx, argv...); err != nil {
			return fmt.Errorf("%s tailscale-calico rule %v: %w", n.Name, argv, err)
		}
	}
	return nil
}

func tailscaleCalicoRuleCommands() [][]string {
	return [][]string{
		{"sh", "-ec", "ip -4 rule del pref 5200 to 100.64.0.0/10 lookup 52 2>/dev/null || true; ip -4 rule add pref 5200 to 100.64.0.0/10 lookup 52"},
		{"sh", "-ec", "ip -6 rule del pref 5200 to fd7a:115c:a1e0::/48 lookup 52 2>/dev/null || true; ip -6 rule add pref 5200 to fd7a:115c:a1e0::/48 lookup 52"},
		{"sh", "-ec", "ip -4 rule del pref 5210 fwmark 0x80000/0xff0000 lookup main 2>/dev/null || true; ip -4 rule add pref 5210 fwmark 0x80000/0xff0000 lookup main"},
		{"sh", "-ec", "ip -6 rule del pref 5210 fwmark 0x80000/0xff0000 lookup main 2>/dev/null || true; ip -6 rule add pref 5210 fwmark 0x80000/0xff0000 lookup main"},
	}
}

// applianceIdentityRoutes gives a LAN appliance its on-link routes to the
// identity prefixes. A container keeps none across a host reboot, so
// recovery gives them back as bring-up first did.
func applianceIdentityRoutes(ctx context.Context, t lab.Topology, name string, node rig.Node) error {
	for _, cidr := range t.IdentityRoutes {
		if _, err := node.Exec(ctx, "ip", "route", "replace", cidr, "dev", node.Interface(0), "scope", "link"); err != nil {
			return fmt.Errorf("%s route to %s: %w", name, cidr, err)
		}
	}
	return nil
}

// identityProxyNDP is a networkd drop-in for netplan's LAN network that
// answers neighbour solicitations on the LAN for the node's IPv6
// identities. IPv6 neighbour discovery is strong-host, so an address on
// vip0 is otherwise unreachable from the rest of the LAN; this is the
// IPv6 counterpart of the weak-host ARP the IPv4 identities rely on.
// Empty for a node with no IPv6 identity.
func identityProxyNDP(n lab.Node, lan string) (string, []byte) {
	var b strings.Builder
	for _, cidr := range n.IdentityDeviceAddresses() {
		addr, _, _ := strings.Cut(cidr, "/")
		if ip := net.ParseIP(addr); ip != nil && ip.To4() == nil {
			b.WriteString("IPv6ProxyNDPAddress=" + ip.String() + "\n")
		}
	}
	if b.Len() == 0 {
		return "", nil
	}
	path := "/etc/systemd/network/10-netplan-" + lan + ".network.d/70-cluster-vip-ndp.conf"
	return path, []byte("[Network]\nIPv6ProxyNDP=yes\n" + b.String())
}

type netplanRoute struct {
	To    string `json:"to"`
	Scope string `json:"scope"`
}

type netplanDevice struct {
	Addresses []string       `json:"addresses,omitempty"`
	Routes    []netplanRoute `json:"routes,omitempty"`
}

type netplanNetwork struct {
	Version      int                      `json:"version"`
	Renderer     string                   `json:"renderer"`
	DummyDevices map[string]netplanDevice `json:"dummy-devices,omitempty"`
	Ethernets    map[string]netplanDevice `json:"ethernets,omitempty"`
}

// identityNetplan is the node's identity device plus on-link routes on its
// LAN link. netplan merges the LAN link with the platform's definition of it.
func identityNetplan(n lab.Node, lan string, routes []string) ([]byte, error) {
	network := netplanNetwork{Version: 2, Renderer: "networkd", Ethernets: map[string]netplanDevice{}}
	if addrs := n.IdentityDeviceAddresses(); len(addrs) > 0 {
		network.DummyDevices = map[string]netplanDevice{lab.IdentityDevice: {Addresses: addrs}}
	}
	link := netplanDevice{}
	for _, cidr := range routes {
		link.Routes = append(link.Routes, netplanRoute{To: cidr, Scope: "link"})
	}
	network.Ethernets[lan] = link
	return yaml.Marshal(map[string]netplanNetwork{"network": network})
}
