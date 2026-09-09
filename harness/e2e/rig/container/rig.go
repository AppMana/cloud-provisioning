package container

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	// The previous lab goes first, and its state with it.
	//
	// A data root is a bind mount, so it outlives the container that
	// used it: a distribution that keeps its cluster state there —
	// k0s keeps etcd's — comes back believing in a cluster whose
	// other members no longer exist. Measured: cp's etcd campaigning
	// at term 3 to two peers from a previous run, never electing a
	// leader, and k0s never reporting itself running. A lab that
	// reuses a previous run's cluster state is not a fresh lab.
	//
	// After the containers are gone, never before: removing a
	// directory a running container has mounted races the mount and
	// leaves it half there.
	_, _, _, _ = r.runner()(ctx, nil, "sudo", "containerlab", "destroy", "-t", r.TopologyPath(), "--cleanup")

	for _, n := range r.Topology.Nodes {
		for _, bind := range n.Binds {
			host, _, _ := strings.Cut(bind, ":")
			dir := filepath.Join(r.WorkDir, host)
			if _, _, _, err := r.runner()(ctx, nil, "sudo", "rm", "-rf", dir); err != nil {
				return fmt.Errorf("clearing %s: %w", dir, err)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("creating the bind directory %s: %w", host, err)
			}
		}
	}
	_, errb, code, err := r.runner()(ctx, nil, "sudo", "containerlab", "deploy", "-t", r.TopologyPath(), "--reconfigure")
	if err != nil {
		return fmt.Errorf("deploying: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("deploying: containerlab exited %d: %s", code, errb)
	}
	return nil
}

// Load carries an image from this host into each node's runtime.
//
// Through a pipe held by this process rather than a temporary file:
// the stream is large, and a file would have to be written, copied
// and cleaned up on every node.
func (r *Rig) Load(ctx context.Context, image string, nodes []string, importArgs []string) error {
	if len(importArgs) == 0 {
		importArgs = []string{"ctr", "-n", "k8s.io", "images", "import", "-"}
	}
	if _, _, code, err := r.runner()(ctx, nil, "docker", "image", "inspect", image); err != nil || code != 0 {
		if _, errb, code, err := r.runner()(ctx, nil, "docker", "pull", "-q", image); err != nil || code != 0 {
			return fmt.Errorf("pulling %s: %v %s", image, err, errb)
		}
	}
	for _, name := range nodes {
		saved, _, code, err := r.runner()(ctx, nil, "docker", "save", image)
		if err != nil || code != 0 {
			return fmt.Errorf("saving %s: %v", image, err)
		}
		node := r.Node(name)
		if _, err := node.Pipe(ctx, bytes.NewReader(saved), importArgs...); err != nil {
			return fmt.Errorf("importing %s into %s: %w", image, name, err)
		}
	}
	return nil
}

// Down destroys the topology.
func (r *Rig) Down(ctx context.Context) error {
	_, errb, code, err := r.runner()(ctx, nil, "sudo", "containerlab", "destroy", "-t", r.TopologyPath(), "--cleanup")
	if err != nil {
		return fmt.Errorf("destroying: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("destroying: containerlab exited %d: %s", code, errb)
	}
	return nil
}
