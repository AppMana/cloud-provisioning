// Package vm runs cluster nodes as virtual machines.
//
// The authoritative rig, and the only one that reaches a boot. Each
// node has a kernel of its own, a bootloader, an init, and a real
// cloud-init that reads the userdata the product rendered — none of
// which a container can offer, and all of which the product depends
// on in production.
//
// Two things about it are not obvious and both were established by
// measurement rather than assumption.
//
// The management interface is one of the lab's own links, on a bridge
// shared with nothing. containerlab's management network is a single
// L2 for every node in a lab, so a topology modelling a private site
// and two separate clouds cannot use it: it would hand every remote a
// path to the site that the routers do not explain, and the isolation
// the lab exists to prove would not be real. vrnetlab assumes that
// network exists and waits for one interface more than the lab
// provides, so the fork takes VR_MGMT_IS_A_LINK to say otherwise.
//
// The guest is reached through its wrapper, not from this host. It
// sits behind qemu's usermode NAT with its ports forwarded onto the
// wrapper's own loopback, so a command runs as: enter the wrapper,
// then ssh to 127.0.0.1. That keeps the control channel off every
// segment the lab models.
package vm

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Guest is the account the launcher creates.
const Guest = "sysadmin"

// KeyPath is where each wrapper keeps the key for its own guest.
const KeyPath = "/tmp/cldt-guest-key"

// Runner executes a command on this host.
type Runner func(ctx context.Context, stdin io.Reader, argv ...string) (stdout, stderr []byte, code int, err error)

// Node is one machine.
type Node struct {
	node    lab.Node
	labName string
	run     Runner
}

// Wrapper is the container the machine runs inside.
func (n *Node) Wrapper() string { return "clab-" + n.labName + "-" + n.node.Name }

// Name is the topology's name for the node.
func (n *Node) Name() string { return n.node.Name }

// ssh builds the command that reaches the guest.
//
// Through the wrapper and then to loopback: the guest's ports are
// forwarded there by qemu, and this host has no route to the guest at
// all — which is the point, because every route this host had to a
// node would be a path the lab's own segments do not explain.
func (n *Node) ssh(stdin bool, argv ...string) []string {
	full := []string{"docker", "exec"}
	if stdin {
		full = append(full, "-i")
	}
	full = append(full, n.Wrapper(), "ssh",
		"-i", KeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		Guest+"@127.0.0.1", "--")
	// One quoted word, not many.
	//
	// ssh does not take an argv: it joins whatever it is given and
	// hands the result to a shell on the far side, which splits it
	// again. Passing the words through would let that shell find
	// meaning in them that the caller never wrote — a redirect in
	// "sh -c 'cat > /usr/local/bin/k0s'" was performed by the login
	// shell as the unprivileged user, and failed with a permission
	// denied on a command that had asked for root.
	//
	// Quoting here is what keeps the promise the interface makes,
	// that a value containing a space or a quote cannot become two
	// words, across a transport that would otherwise break it.
	quoted := make([]string, 0, len(argv)+1)
	quoted = append(quoted, "sudo")
	for _, word := range argv {
		quoted = append(quoted, shellQuote(word))
	}
	return append(full, strings.Join(quoted, " "))
}

func (n *Node) Exec(ctx context.Context, argv ...string) ([]byte, error) {
	return n.Pipe(ctx, nil, argv...)
}

func (n *Node) Pipe(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	full := n.ssh(stdin != nil, argv...)
	out, errb, code, err := n.run(ctx, stdin, full...)
	if err != nil {
		return out, fmt.Errorf("%s: running %v: %w", n.Name(), argv, err)
	}
	if code != 0 {
		return out, &rig.ExitError{Node: n.Name(), Argv: argv, Code: code, Stderr: errb}
	}
	return out, nil
}

// Put writes a file onto the guest.
func (n *Node) Put(ctx context.Context, src io.Reader, dst string, mode fs.FileMode) error {
	if i := strings.LastIndex(dst, "/"); i > 0 {
		if _, err := n.Exec(ctx, "mkdir", "-p", dst[:i]); err != nil {
			return fmt.Errorf("creating %s: %w", dst[:i], err)
		}
	}
	if _, err := n.Pipe(ctx, src, "sh", "-c", "cat > "+shellQuote(dst)); err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	if _, err := n.Exec(ctx, "chmod", strconv.FormatUint(uint64(mode.Perm()), 8), dst); err != nil {
		return fmt.Errorf("setting mode on %s: %w", dst, err)
	}
	return nil
}

// Cut takes the machine's data links down and leaves it running.
//
// Its data links only: the management link is how this harness
// reaches it at all, and taking that away would be taking away the
// ability to observe rather than modelling a pulled cable. A real
// machine losing its data network keeps whatever out-of-band access
// its operator has, which is exactly this.
func (n *Node) Cut(ctx context.Context) error {
	for _, i := range n.dataInterfaces() {
		if _, err := n.Exec(ctx, "ip", "link", "set", i, "down"); err != nil {
			return err
		}
	}
	return nil
}

// Restore brings the data links back with their addresses.
func (n *Node) Restore(ctx context.Context) error {
	for idx, i := range n.dataInterfaces() {
		if _, err := n.Exec(ctx, "ip", "link", "set", i, "up"); err != nil {
			return err
		}
		if addr := n.node.Interfaces[idx].Address; addr != "" {
			if _, err := n.Exec(ctx, "ip", "addr", "replace", addr, "dev", i); err != nil {
				return err
			}
		}
	}
	return nil
}

// Kill stops the machine ungracefully: the wrapper goes, and qemu
// with it. No shutdown, no goodbye, which is what a power event is.
func (n *Node) Kill(ctx context.Context) error {
	_, errb, code, err := n.run(ctx, nil, "docker", "kill", n.Wrapper())
	if err != nil {
		return err
	}
	if code != 0 {
		return &rig.ExitError{Node: n.Name(), Argv: []string{"docker", "kill"}, Code: code, Stderr: errb}
	}
	return nil
}

// Boot starts the machine again, which boots it: a real bootloader,
// a real init, and the whole start-up ordering a container never has.
func (n *Node) Boot(ctx context.Context) error {
	_, errb, code, err := n.run(ctx, nil, "docker", "start", n.Wrapper())
	if err != nil {
		return err
	}
	if code != 0 {
		return &rig.ExitError{Node: n.Name(), Argv: []string{"docker", "start"}, Code: code, Stderr: errb}
	}
	return nil
}

// Userdata is not implemented here on purpose.
//
// A machine's userdata is read by its own cloud-init at first boot,
// from the seed its platform gave it — so it has to be in place
// before the machine starts, not handed to a running one. The rig
// writes it into the node's seed when the lab is built; a caller that
// reaches this has tried to bootstrap a machine that is already
// running, which on a real platform is not a thing that happens.
func (n *Node) Userdata(ctx context.Context, cloudConfig []byte) error {
	return fmt.Errorf("%s: a machine reads its userdata at first boot, from the seed its platform gave it; "+
		"it cannot be handed to one that is already running", n.Name())
}

// Interface is what the guest calls the lab's nth link.
func (n *Node) Interface(nth int) string {
	if nth < 0 || nth >= len(n.node.Interfaces) {
		return ""
	}
	return GuestInterface(nth)
}

// dataInterfaces are the guest's names for the lab's links.
//
// The guest does not see them as eth1..N: the kernel names them for
// the virtual bus it finds them on, and the management link is the
// first. So the lab's Nth link is the guest's Nth data interface, in
// the order qemu attached them.
func (n *Node) dataInterfaces() []string {
	out := make([]string, 0, len(n.node.Interfaces))
	for i := range n.node.Interfaces {
		out = append(out, GuestInterface(i))
	}
	return out
}

// GuestInterface names the guest's interface for the lab's nth data
// link. Measured on a booted guest: lo, enp1s0 (management), ens2
// (the first data link).
func GuestInterface(n int) string { return "ens" + strconv.Itoa(n+2) }

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
