package vm

import (
	"context"
	"fmt"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"strings"
)

// ProveHardware checks physical Ethernet devices through sysfs. CNI bridges and
// WireGuard interfaces must not be mistaken for additional guest NICs.
func (r *Rig) ProveHardware(ctx context.Context) error { return r.proveHardware(ctx, false) }

// ProveExistingHardware permits an unallocated remote slot after claim deletion.
func (r *Rig) ProveExistingHardware(ctx context.Context) error { return r.proveHardware(ctx, true) }
func (r *Rig) proveHardware(ctx context.Context, allowStopped bool) error {
	for _, node := range r.Topology.Nodes {
		if !node.IsClusterNode() {
			continue
		}
		if allowStopped && node.Role == lab.Remote {
			state, stderr, code, err := r.runner()(ctx, nil, "docker", "inspect", "--format", "{{.State.Running}}", "clab-"+r.Topology.Name+"-"+node.Name)
			if err != nil || code != 0 {
				return fmt.Errorf("observing %s: %v %s", node.Name, err, stderr)
			}
			if strings.TrimSpace(string(state)) == "false" {
				continue
			}
		}
		out, err := r.Node(node.Name).Exec(ctx, "sh", "-c", `for n in /sys/class/net/*; do test -e "$n/device" && basename "$n"; done; true`)
		if err != nil {
			return err
		}
		names := strings.Fields(string(out))
		if len(names) != 1 || names[0] != GuestInterface(0) {
			return fmt.Errorf("%s physical Ethernet devices %v; require exactly %s", node.Name, names, GuestInterface(0))
		}
		_, stderr, code, err := r.runner()(ctx, nil, "docker", "exec", "clab-"+r.Topology.Name+"-"+node.Name, "sh", "-c", `for p in /proc/[0-9]*/comm; do case "$(cat "$p" 2>/dev/null)" in qemu-system-*) test -e "${p%comm}fd" && ls -l "${p%comm}fd" | grep -q 'kvm-vm' && exit 0;; esac; done; exit 1`)
		if err != nil || code != 0 {
			return fmt.Errorf("%s has no observed KVM VM: %v %s", node.Name, err, stderr)
		}
	}
	return nil
}
