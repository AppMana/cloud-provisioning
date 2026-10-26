package container

import (
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// The nodes satisfy the interface every stage reaches them through.
// Asserted here so that a change to it shows up as a compile error in
// this package rather than at the one call site that happens to use the
// method that went missing.
var _ rig.Node = (*Node)(nil)

// Rig is the view of a VM lab's network appliances: the routers, cloud
// edges and bastion that stay containers. The VM rig owns deployment; this
// view only reaches the appliances through the lab's own session.
type Rig struct {
	Topology lab.Topology
	// WorkDir holds the parent lab's session reference.
	WorkDir string
	// Run executes commands on this host. Zero value means Exec.
	Run Runner
	// Runtime is the parent lab's runtime.
	Runtime rig.Runtime
	// RuntimeKind selects the parent lab's session.
	RuntimeKind string
}

func (r *Rig) runner() Runner {
	if r.Run == nil {
		return Exec
	}
	return r.Run
}

// Node returns one node by its topology name. A name the topology
// does not have is a bug in the harness rather than a condition to
// handle, so it panics rather than returning an interface that fails
// later somewhere less obvious.
func (r *Rig) Node(name string) rig.Node {
	return &Node{node: r.Topology.MustNode(name), labName: r.Topology.Name, run: r.runner(), rig: r}
}
