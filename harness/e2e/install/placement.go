package install

import (
	"context"
	"fmt"
)

// Placement is which site nodes terminate tunnels.
//
// This is the one value a placement row changes, and the claim it
// tests is that changing it is invisible from the pod network: a node
// with no tunnel reaches a remote by transiting one that has one, so
// every ordered pair passes whatever the selector says. A row that
// fails is that claim being wrong.
//
// It is also the change under which the mesh has historically been
// most fragile, because it moves traffic between paths while both
// exist: a node that stops being an endpoint has to keep carrying
// what it carried until the remotes have learned the new way round.
type Placement struct {
	// Name is what a row calls this placement.
	Name string
	// Endpoints is the selector, in the form the chart takes: a label
	// selector, a set such as "kubernetes.io/hostname in (w1,w2)", or
	// the word "all".
	Endpoints string
}

// Common placements, in the order a campaign walks them: the number
// of endpoints only grows, so a remote that has joined stays joined
// and what changes between rows is which site nodes hold tunnels.
var (
	OnControlPlane = Placement{Name: "control-plane", Endpoints: "kubernetes.io/hostname=cp"}
	OnOneWorker    = Placement{Name: "one-worker", Endpoints: "kubernetes.io/hostname=w1"}
	OnTwoWorkers   = Placement{Name: "two-workers", Endpoints: "kubernetes.io/hostname in (w1,w2)"}
	OnAllNodes     = Placement{Name: "all-nodes", Endpoints: "all"}
)

// Placements is the walk a campaign takes.
var Placements = []Placement{OnControlPlane, OnOneWorker, OnTwoWorkers, OnAllNodes}

// PlacementNamed returns one by name.
func PlacementNamed(name string) (Placement, error) {
	for _, p := range Placements {
		if p.Name == name {
			return p, nil
		}
	}
	var have []string
	for _, p := range Placements {
		have = append(have, p.Name)
	}
	return Placement{}, fmt.Errorf("no placement named %q (have %v)", name, have)
}

// Move changes which site nodes terminate tunnels, by upgrading the
// release the way an operator would.
//
// Through helm, with the same chart and the same values but one: a
// harness that edited a Deployment or restarted a pod to change this
// would be testing its own ability to move a tunnel rather than the
// product's.
func (p *Product) Move(ctx context.Context, to Placement, opts Options, dialerSHA string) error {
	opts.TunnelEndpoints = to.Endpoints
	if err := p.Install(ctx, opts, dialerSHA); err != nil {
		return fmt.Errorf("moving the tunnels to %s: %w", to.Name, err)
	}
	return nil
}
