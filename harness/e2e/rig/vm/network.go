package vm

import (
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// Resolver is the nameserver a machine is given.
//
// On its lab interface, never on management. A machine resolving
// through the management NIC would be reaching a service by a path no
// router in this topology explains, which is the same borrowed route
// the management default gateway used to provide — and with it a node
// could pull images while the segments it is supposed to depend on
// were down, so an outage row would measure nothing.
//
// It has to be given at all because a machine has no images but the
// ones it pulls: k0s runs its node-local balancer as a static pod, so
// a worker that cannot resolve a registry cannot start the balancer,
// cannot reach the API through it, and never registers. A container
// image ships with those layers already in it and needed no resolver,
// which is why removing the borrowed one broke only machines.
const Resolver = "9.9.9.9"

// NetworkConfig renders the platform's network configuration for one
// machine, in the form cloud-init reads.
//
// Because a platform addresses an instance before its userdata runs,
// and the whole point of a remote is that its userdata dials the site
// the moment it runs. Configured afterwards by the harness reaching
// in over the management path, a machine spends its first boot with
// no address on the segment its own bootstrap needs — the tunnel it
// tries to raise has nowhere to leave from, and what the row would
// then be measuring is how patiently the product retries.
//
// It is also what a reboot row requires. A machine that comes back
// must find a NIC, an address and a gateway already there, provided
// by its platform and by nothing else, so that everything above them
// is the product rebuilding its own state.
func NetworkConfig(n lab.Node) (string, error) {
	var b strings.Builder
	b.WriteString("version: 2\n")
	b.WriteString("ethernets:\n")

	if len(n.Interfaces) != 1 {
		return "", fmt.Errorf("%s must have exactly one Ethernet link", n.Name)
	}
	via, ok := lab.Gateway(n.Interfaces[0].Segment)
	if !ok {
		return "", fmt.Errorf("%s is on %s, which has no edge", n.Name, n.Interfaces[0].Segment)
	}
	for idx, i := range n.Interfaces {
		fmt.Fprintf(&b, "  %s:\n", GuestInterface(idx))
		if i.Address != "" {
			fmt.Fprintf(&b, "    addresses: [%s]\n", i.Address)
		}
		if idx == 0 {
			b.WriteString("    routes:\n")
			b.WriteString("      - to: default\n")
			fmt.Fprintf(&b, "        via: %s\n", via)
			b.WriteString("    nameservers:\n")
			fmt.Fprintf(&b, "      addresses: [%s]\n", Resolver)
		}
	}
	return b.String(), nil
}
