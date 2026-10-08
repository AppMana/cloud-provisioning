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
			continue
		}
		if err := applianceIdentityRoutes(ctx, t, n.Name, node); err != nil {
			return err
		}
	}
	return nil
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
	for _, cidr := range n.Identity {
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
	if len(n.Identity) > 0 {
		network.DummyDevices = map[string]netplanDevice{lab.IdentityDevice: {Addresses: n.Identity}}
	}
	link := netplanDevice{}
	for _, cidr := range routes {
		link.Routes = append(link.Routes, netplanRoute{To: cidr, Scope: "link"})
	}
	network.Ethernets[lan] = link
	return yaml.Marshal(map[string]netplanNetwork{"network": network})
}
