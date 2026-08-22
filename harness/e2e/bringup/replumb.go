package bringup

import (
	"context"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

// Replumb gives a machine back exactly what a platform would: its
// NIC, its address, its gateway, and nothing else.
//
// For either way of losing it. A killed machine needs the host side
// of its veth rebuilt; a machine whose cable was pulled still has
// one, but a link that goes down takes its routes with it and the
// kernel does not put them back — on a real machine something else
// does, and here that something is this. A node restored with an
// address and no gateway is reachable on its own segment and nowhere
// else, which reads as the cluster failing to readmit it.
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
	if err := wait.Until(ctx, 2*time.Minute, victim+" never came back", func(ctx context.Context) error {
		_, err := r.Node(victim).Exec(ctx, "true")
		return err
	}); err != nil {
		return err
	}

	for _, i := range node.Interfaces {
		// Only when the NIC is actually gone.
		//
		// A machine that was killed lost the host side of its veth
		// with its namespace, and needs one back. A machine whose
		// cable was pulled still has it, and tearing it down to
		// rebuild it would be doing more than the platform does — and
		// would take away an interface the node is still using.
		if _, err := r.Node(victim).Exec(ctx, "ip", "link", "show", i.Name); err == nil {
			continue
		}
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
