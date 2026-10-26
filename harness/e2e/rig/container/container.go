// Package container reaches a VM lab's network appliances: the routers,
// cloud edges and bastion that run as containers beside the machines.
// Commands and files go through the lab's own Labcontainers session;
// stopping and starting an appliance uses the Docker CLI.
package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	labclient "github.com/appmana/labcontainers/pkg/client"
)

// Runner executes a command on this host. Injectable so that what the
// rig asks the host to do can be tested without a host that will do
// it.
type Runner func(ctx context.Context, stdin io.Reader, argv ...string) (stdout, stderr []byte, code int, err error)

// Exec is the real runner.
func Exec(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.Bytes(), errb.Bytes(), exitErr.ExitCode(), nil
	}
	return out.Bytes(), errb.Bytes(), 0, err
}

// Node is one appliance.
type Node struct {
	node    lab.Node
	labName string
	run     Runner
	rig     *Rig
}

// Container is the name docker knows this node by. The topology calls
// it cp; docker calls it clab-cldt-cp.
func (n *Node) Container() string { return "clab-" + n.labName + "-" + n.node.Name }

// Name is the topology's name for the node.
func (n *Node) Name() string { return n.node.Name }

// Interface is what a container calls the lab's nth link, which is
// what the topology calls it: containerlab names them directly.
func (n *Node) Interface(nth int) string {
	if nth < 0 || nth >= len(n.node.Interfaces) {
		return ""
	}
	return n.node.Interfaces[nth].Name
}

func (n *Node) Exec(ctx context.Context, argv ...string) ([]byte, error) {
	return n.Pipe(ctx, nil, argv...)
}

func (n *Node) Pipe(ctx context.Context, stdin io.Reader, argv ...string) (out []byte, runErr error) {
	c, session, err := n.openSDK(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { runErr = errors.Join(runErr, c.Close()) }()
	return session.Node(n.Name()).Commands().Pipe(ctx, stdin, argv...)
}

// Put writes a file onto the node.
//
// The parent directory is created first. cloud-init makes it; a shell
// redirect does not, and the first file written to /etc/wg-dialer is
// always the one creating it.
func (n *Node) Put(ctx context.Context, src io.Reader, dst string, mode fs.FileMode) (runErr error) {
	if i := strings.LastIndex(dst, "/"); i > 0 {
		if _, err := n.Exec(ctx, "mkdir", "-p", dst[:i]); err != nil {
			return fmt.Errorf("creating %s: %w", dst[:i], err)
		}
	}
	c, session, err := n.openSDK(ctx)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, c.Close()) }()
	return session.Node(n.Name()).Commands().Put(ctx, src, dst, mode)
}

func (n *Node) openSDK(ctx context.Context) (*labclient.Client, *labclient.Session, error) {
	if n.rig == nil || n.rig.Runtime == nil || n.rig.RuntimeKind == "" {
		return nil, nil, fmt.Errorf("%s: appliances are reached only through their lab's session", n.Name())
	}
	return rig.OpenRuntime(ctx, n.rig.Runtime, n.rig.WorkDir, n.rig.RuntimeKind)
}

// Cut takes every data interface down, leaving the machine running.
// Not docker network disconnect: the interfaces are containerlab's,
// and what a pulled cable does is stop the link, not remove it.
func (n *Node) Cut(ctx context.Context) error {
	for idx := range n.node.Interfaces {
		if _, err := n.Exec(ctx, "ip", "link", "set", n.Interface(idx), "down"); err != nil {
			return err
		}
	}
	return nil
}

// Restore brings the interfaces back and re-adds their addresses. An
// address does not survive a link going down on every kernel path
// that can take it away, so it is replaced rather than assumed.
func (n *Node) Restore(ctx context.Context) error {
	for idx, i := range n.node.Interfaces {
		if _, err := n.Exec(ctx, "ip", "link", "set", n.Interface(idx), "up"); err != nil {
			return err
		}
		if i.Address != "" {
			if _, err := n.Exec(ctx, "ip", "addr", "replace", i.Address, "dev", n.Interface(idx)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Kill stops the container ungracefully.
func (n *Node) Kill(ctx context.Context) error {
	_, errb, code, err := n.run(ctx, nil, "docker", "kill", n.Container())
	if err != nil {
		return err
	}
	if code != 0 {
		return &rig.ExitError{Node: n.Name(), Argv: []string{"docker", "kill"}, Code: code, Stderr: errb}
	}
	return nil
}

// Boot starts the container again.
//
// A container that comes back has lost the veth that joined it to its
// bridge, because the host side went with the netns. Recreating it is
// this rig standing in for a platform handing a machine its NIC back,
// and it is the reason the reboot rows are a fair test here at all:
// the node gets an interface, an address and a gateway, and must
// rebuild everything else itself.
func (n *Node) Boot(ctx context.Context) error {
	if _, errb, code, err := n.run(ctx, nil, "docker", "start", n.Container()); err != nil || code != 0 {
		if err != nil {
			return err
		}
		return &rig.ExitError{Node: n.Name(), Argv: []string{"docker", "start"}, Code: code, Stderr: errb}
	}
	return nil
}

// Userdata is refused: appliances are not machines a product provisions,
// so no rendered cloud-config is ever meant for one.
func (n *Node) Userdata(context.Context, []byte) error {
	return fmt.Errorf("%s: appliances take no userdata", n.Name())
}
