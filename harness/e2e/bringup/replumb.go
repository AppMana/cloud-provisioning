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

	// The links first, and from the host, before anything expects the
	// node to answer.
	//
	// Both halves of that ordering were wrong and both only showed on
	// machines. The check ran inside the node against the topology's
	// name for the link, which a machine does not use — its kernel
	// names interfaces for the bus it finds them on — so it never
	// found one that was there. And it ran after waiting for the node
	// to answer, which on a machine it cannot do until it has booted,
	// which it cannot do until it has the links: the wait and the work
	// that would end it were the wrong way round.
	//
	// Asking the host settles both. The host side of a veth dies with
	// the namespace it was joined to, so its absence is exactly the
	// question being asked — the machine was taken away and needs a
	// NIC back — while a machine whose cable was merely pulled still
	// has it, and rebuilding that would take away an interface the
	// node is still using. It is also the only place that can answer
	// before the node is up.
	for _, i := range node.Interfaces {
		endpoint := lab.EndpointName(i.Segment, node)
		if _, err := h.Run(ctx, "ip", "link", "show", endpoint); err == nil {
			continue
		}
		// A stale host-side veth outlives the namespace it belonged
		// to, and a new one cannot take a name that already exists.
		_, _ = h.Run(ctx, "sudo", "ip", "link", "del", endpoint)
		if _, err := h.Run(ctx, "sudo", "containerlab", "tools", "veth", "create",
			"-a", "clab-"+t.Name+"-"+victim+":"+i.Name,
			"-b", "bridge:"+i.Segment+":"+endpoint); err != nil {
			return fmt.Errorf("re-plumbing %s: %w", i.Segment+":"+victim, err)
		}
	}

	// Now it can be expected to answer: a machine needs its links to
	// finish booting, and a container needs them to be reachable.
	if err := wait.Until(ctx, BackTimeout, victim+" never came back", func(ctx context.Context) error {
		_, err := r.Node(victim).Exec(ctx, "true")
		return err
	}); err != nil {
		return err
	}

	if err := Configure(ctx, lab.Topology{Name: t.Name, Segments: t.Segments,
		Nodes: []lab.Node{node}}, r, h); err != nil {
		return fmt.Errorf("re-addressing %s: %w", victim, err)
	}
	return nil
}

// BackTimeout is how long a node has to come back.
//
// Long enough for a boot, because on the authoritative rig that is
// what coming back is: firmware, a bootloader, a kernel and an init,
// none of which a container has.
const BackTimeout = 8 * time.Minute
