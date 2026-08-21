package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// The rig and its nodes satisfy the interfaces every stage reaches
// them through. Asserted here so that a change to either shows up as
// a compile error in this package rather than at the one call site
// that happens to use the method that went missing.
var (
	_ rig.Rig  = (*Rig)(nil)
	_ rig.Node = (*Node)(nil)
)

// Rig is the containerised lab.
type Rig struct {
	Topology lab.Topology
	// WorkDir is where the generated topology and any state the rig
	// needs on this host are written.
	WorkDir string
	// Run executes commands on this host. Zero value means Exec.
	Run Runner
}

// New returns a rig for the given topology.
func New(topo lab.Topology, workDir string) *Rig {
	return &Rig{Topology: topo, WorkDir: workDir, Run: Exec}
}

func (r *Rig) Kind() string { return lab.Container.String() }

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
	return &Node{node: r.Topology.MustNode(name), labName: r.Topology.Name, run: r.runner()}
}

// TopologyPath is where the generated containerlab file is written.
func (r *Rig) TopologyPath() string {
	return filepath.Join(r.WorkDir, r.Topology.Name+".clab.yml")
}

// Up writes the topology and deploys it.
func (r *Rig) Up(ctx context.Context) error {
	yaml, err := r.Topology.ContainerlabYAML(lab.Container)
	if err != nil {
		return fmt.Errorf("rendering the topology: %w", err)
	}
	if err := os.MkdirAll(r.WorkDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(r.TopologyPath(), []byte(yaml), 0o644); err != nil {
		return err
	}
	_, errb, code, err := r.runner()(ctx, nil, "containerlab", "deploy", "-t", r.TopologyPath(), "--reconfigure")
	if err != nil {
		return fmt.Errorf("deploying: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("deploying: containerlab exited %d: %s", code, errb)
	}
	return nil
}

// Down destroys the topology.
func (r *Rig) Down(ctx context.Context) error {
	_, errb, code, err := r.runner()(ctx, nil, "containerlab", "destroy", "-t", r.TopologyPath(), "--cleanup")
	if err != nil {
		return fmt.Errorf("destroying: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("destroying: containerlab exited %d: %s", code, errb)
	}
	return nil
}
