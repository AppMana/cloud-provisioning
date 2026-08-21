package bringup

import (
	"context"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Replumb gives a machine back exactly what a platform would: its
// NIC, its address, its gateway, and nothing else.
//
// That "nothing else" is the whole point of a reboot row. The tunnel,
// the routes and the cluster membership have to be rebuilt by what
// the node itself runs at boot — the host unit raising the tunnel
// from its cached peer list while the cluster is still unreachable,
// because the cluster is on the far side of the tunnel it is raising.
// A harness that restored any of that would be proving its own
// ability to restore it.
//
// A container that comes back has lost the veth that joined it to its
// bridge, because the host side went with the namespace. Recreating
// it is this rig standing in for a platform handing back a NIC.
func Replumb(ctx context.Context, t lab.Topology, r rig.Rig, h Host, victim string) error {
	node := t.MustNode(victim)

	// Wait for the machine to be running again before touching it.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := r.Node(victim).Exec(ctx, "true"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s never came back", victim)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}

	for _, i := range node.Interfaces {
		endpoint := i.Segment + ":" + victim
		// A stale host-side veth outlives the namespace it belonged
		// to, and a new one cannot take a name that already exists.
		_, _ = h.Run(ctx, "sudo", "ip", "link", "del", lab.EndpointName(i.Segment, node))
		if _, err := h.Run(ctx, "sudo", "containerlab", "tools", "veth", "create",
			"-a", "clab-"+t.Name+"-"+victim+":"+i.Name,
			"-b", "bridge:"+i.Segment+":"+lab.EndpointName(i.Segment, node)); err != nil {
			return fmt.Errorf("re-plumbing %s: %w", endpoint, err)
		}
	}
	if err := Configure(ctx, lab.Topology{Name: t.Name, Segments: t.Segments,
		Nodes: []lab.Node{node}}, r, h); err != nil {
		return fmt.Errorf("re-addressing %s: %w", victim, err)
	}
	return nil
}
