package vm

import (
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// ManagementAddress is what qemu's usermode network hands the guest
// on its management NIC, and ManagementInterface is what the guest
// calls it.
const (
	ManagementAddress   = "10.0.0.15/24"
	ManagementInterface = "enp1s0"
)

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
//
// The management NIC carries no default route. The lab's segments are
// the only paths this topology explains, and a machine that could
// leave by its management interface would leave by a path no router
// in the lab accounts for, which is the isolation the lab exists to
// prove.
func NetworkConfig(n lab.Node) (string, error) {
	var b strings.Builder
	b.WriteString("version: 2\n")
	b.WriteString("ethernets:\n")
	fmt.Fprintf(&b, "  %s:\n", ManagementInterface)
	fmt.Fprintf(&b, "    addresses: [%s]\n", ManagementAddress)

	if len(n.Interfaces) == 0 {
		return "", fmt.Errorf("%s has no link to be addressed on", n.Name)
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
		}
	}
	return b.String(), nil
}
