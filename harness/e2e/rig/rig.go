// Package rig is what a node is made of.
//
// Every stage of the harness reaches nodes through this interface and
// never through docker or ssh directly, so that a row runs unchanged
// on either rig. That is the whole point: a container row and a VM
// row must differ in what a node *is* and in nothing else, or the two
// are not comparable and a divergence between them says nothing.
//
// The seam it replaces was three shell helpers copied into nine
// files:
//
//	c()       { echo "clab-$LAB-$1"; }
//	in_node() { docker exec "$(c "$1")" "${@:2}"; }
//	k()       { in_node bastion kubectl "$@"; }
package rig

import (
	"context"
	"fmt"
	"io"
	"io/fs"
)

// Node is one machine in the lab.
type Node interface {
	// Name is the node's name in the topology (cp, w1, remote1), not
	// whatever the rig calls the thing implementing it.
	Name() string

	// Interface is what this node calls the lab's nth link.
	//
	// A container calls it what the topology does. A machine does not:
	// its kernel names interfaces for the bus it finds them on, so the
	// lab's first link is ens2 inside the guest. Configuring a machine
	// with the topology's names silently addresses nothing.
	Interface(nth int) string

	// Exec runs argv on the node and returns its standard output. The
	// words are passed through as they are: nothing here goes through
	// a shell unless the caller asks for one by name, so a value
	// containing a space or a quote cannot become two words.
	Exec(ctx context.Context, argv ...string) ([]byte, error)

	// Pipe is Exec with something on standard input.
	//
	// It is separate because forgetting it is silent: the bash
	// harness reached kubectl apply without docker exec -i more than
	// once, and a manifest read from an empty stdin applies nothing
	// and reports success.
	Pipe(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error)

	// Put writes a file onto the node, creating parent directories.
	Put(ctx context.Context, src io.Reader, dst string, mode fs.FileMode) error

	// Cut takes the node's network away without stopping it: a pulled
	// cable, which from everywhere else looks like silence. The
	// machine keeps running against it, which is what the link outage
	// rows measure.
	Cut(ctx context.Context) error

	// Restore puts the node's network back.
	Restore(ctx context.Context) error

	// Kill stops the machine ungracefully. No shutdown, no goodbye:
	// what a power event looks like.
	Kill(ctx context.Context) error

	// Boot brings the machine back with only what a platform
	// provides: a NIC, an address, a gateway. Everything else must be
	// rebuilt by what the node itself runs at boot, which is the
	// invariant the reboot rows exist to prove.
	Boot(ctx context.Context) error

	// Userdata hands the node its first-boot configuration.
	//
	// This is where the rigs genuinely differ, and the difference is
	// the reason the VM rig exists. A machine boots a real cloud-init
	// and these bytes are never interpreted by the harness; a
	// container has no boot, so the harness applies the document
	// itself and can only apply what it implements.
	Userdata(ctx context.Context, cloudConfig []byte) error
}

// Nodes resolves machines for measurement without granting provisioning or
// teardown operations. A fleet may combine local VMs and CAPA-owned instances.
type Nodes interface {
	Node(name string) Node
}

// Rig builds and tears down a lab.
type Rig interface {
	Nodes
	// Up deploys the topology.
	Up(ctx context.Context) error
	// Down destroys the topology.
	Down(ctx context.Context) error
	// Kind names the rig for a log line and a row label.
	Kind() string
}

// Fleet overlays explicitly bound remote machines on an existing site. It
// deliberately implements Nodes, not Rig: CAPA owns the remote lifecycle.
type Fleet struct {
	Site    Nodes
	Remotes map[string]Node
}

func (f *Fleet) Node(name string) Node {
	if n, ok := f.Remotes[name]; ok {
		return n
	}
	return f.Site.Node(name)
}

// ExitError is a command that ran and failed. Stderr is carried
// because a harness that reports only an exit status makes every
// failure a second investigation.
type ExitError struct {
	Node   string
	Argv   []string
	Code   int
	Stderr []byte
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("%s: %v exited %d", e.Node, e.Argv, e.Code)
	if len(e.Stderr) > 0 {
		msg += ": " + string(e.Stderr)
	}
	return msg
}
