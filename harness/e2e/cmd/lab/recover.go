package main

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/harness/e2e/bringup"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// recoveryOrder lists every node to re-plumb after this host lost its
// segments: the appliances first, because each machine's default route
// runs through its router or edge and the bastion carries every session
// to a machine, then the machines that boot from their existing disks.
func recoveryOrder(t lab.Topology) []lab.Node {
	var appliances, machines []lab.Node
	for _, n := range t.Nodes {
		if n.IsClusterNode() {
			machines = append(machines, n)
		} else {
			appliances = append(appliances, n)
		}
	}
	return append(appliances, machines...)
}

// rigNodes keeps the registered Nodes the rig can reach: the ones the
// topology defines. A site that also carries real cloud workers registers
// Nodes the rig has no wrapper for, and asking it about one is a panic;
// those Nodes are still observed through the API.
func rigNodes(t lab.Topology, registered []string) []string {
	var out []string
	for _, name := range registered {
		for _, n := range t.Nodes {
			if n.Name == name {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// powerCycler is a machine the rig can take away and hand back on the
// same disk.
type powerCycler interface {
	Kill(ctx context.Context) error
	Boot(ctx context.Context) error
}

// recoverHostSide gives a lab back what a host reboot took: the bridges,
// masquerading and return routes on this host, then every wrapper's veth
// and addressing. A wrapper that Docker restarted without its links waits
// for them before launching qemu, so each machine is restarted once its
// bridges exist; the wrapper keeps its writable layer, so the guest boots
// the disk it had. Nothing here touches cluster state: what the guests
// rebuild at boot is what the reboot rows measure.
func recoverHostSide(ctx context.Context, t lab.Topology, r rig.Rig, host bringup.Host) error {
	if err := bringup.PrepareHost(ctx, t, host); err != nil {
		return fmt.Errorf("preparing this host: %w", err)
	}
	for _, n := range recoveryOrder(t) {
		if machine, ok := r.Node(n.Name).(powerCycler); ok && n.IsClusterNode() {
			if err := machine.Kill(ctx); err != nil {
				return fmt.Errorf("stopping %s: %w", n.Name, err)
			}
			if err := machine.Boot(ctx); err != nil {
				return fmt.Errorf("booting %s from its disk: %w", n.Name, err)
			}
		}
		if err := bringup.Replumb(ctx, t, r, host, n.Name); err != nil {
			return err
		}
		fmt.Printf("  %s is back\n", n.Name)
	}
	return nil
}
