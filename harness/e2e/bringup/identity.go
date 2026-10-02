package bringup

import (
	"bytes"
	"context"
	"fmt"

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
			if _, err := node.Exec(ctx, "netplan", "apply"); err != nil {
				return fmt.Errorf("%s applying identity configuration: %w", n.Name, err)
			}
			continue
		}
		for _, cidr := range t.IdentityRoutes {
			if _, err := node.Exec(ctx, "ip", "route", "replace", cidr, "dev", node.Interface(0), "scope", "link"); err != nil {
				return fmt.Errorf("%s route to %s: %w", n.Name, cidr, err)
			}
		}
	}
	return nil
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
