// Package container runs cluster nodes as containers.
//
// The fast rig. Its kernel is real — WireGuard, netfilter, ip rules
// and routing tables are exercised per network namespace against the
// host's own kernel, and every defect the matrix has found was a
// genuine kernel-datapath bug rather than an artifact of this. What
// it cannot reach is a boot: no bootloader, no initramfs, no
// first-boot userdata, and one kernel shared by every node.
//
// It drives the docker CLI rather than the engine's Go SDK. The SDK
// is a large dependency for this, and the two things that actually
// matter here are had either way: argv is passed through without a
// shell, and standard input is attached by holding the pipe rather
// than by remembering a flag.
package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/cloudinit"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
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

// Node is one containerised node.
type Node struct {
	node    lab.Node
	labName string
	run     Runner

	// accommodations records what this rig had to change about the
	// last document it applied. See Accommodations.
	accommodations []string
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

func (n *Node) Pipe(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	full := []string{"docker", "exec"}
	if stdin != nil {
		// -i, the flag whose absence is silent: a manifest read from
		// an unattached stdin applies nothing and reports success.
		full = append(full, "-i")
	}
	full = append(full, n.Container())
	full = append(full, argv...)

	out, errb, code, err := n.run(ctx, stdin, full...)
	if err != nil {
		return out, fmt.Errorf("%s: running %v: %w", n.Name(), argv, err)
	}
	if code != 0 {
		return out, &rig.ExitError{Node: n.Name(), Argv: argv, Code: code, Stderr: errb}
	}
	return out, nil
}

// Put writes a file onto the node.
//
// The parent directory is created first. cloud-init makes it; a shell
// redirect does not, and the first file written to /etc/wg-dialer is
// always the one creating it.
func (n *Node) Put(ctx context.Context, src io.Reader, dst string, mode fs.FileMode) error {
	if i := strings.LastIndex(dst, "/"); i > 0 {
		if _, err := n.Exec(ctx, "mkdir", "-p", dst[:i]); err != nil {
			return fmt.Errorf("creating %s: %w", dst[:i], err)
		}
	}
	// Written through a shell redirect held open by this process
	// rather than by docker cp, so that a file whose content is
	// generated does not have to exist on this host first.
	if _, err := n.Pipe(ctx, src, "sh", "-c", "cat > "+shellQuote(dst)); err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	if _, err := n.Exec(ctx, "chmod", strconv.FormatUint(uint64(mode.Perm()), 8), dst); err != nil {
		return fmt.Errorf("setting mode on %s: %w", dst, err)
	}
	return nil
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

// Userdata applies the rendered cloud-config by hand, because this
// rig has no cloud-init to apply it.
//
// This is the container rig's largest fidelity gap and it is narrowed
// rather than hidden: the document is parsed by the same package that
// refuses what it cannot do, and anything skipped is returned so a
// caller can say so out loud.
func (n *Node) Userdata(ctx context.Context, cloudConfig []byte) error {
	doc, err := cloudinit.Parse(cloudConfig)
	if err != nil {
		return fmt.Errorf("%s: %w", n.Name(), err)
	}
	n.accommodations = nil
	for _, f := range doc.WriteFiles {
		if err := n.Put(ctx, strings.NewReader(f.Content), f.Path, fs.FileMode(f.Mode)); err != nil {
			return err
		}
	}
	for _, c := range doc.RunCmd {
		c, why := accommodate(c)
		if why != "" {
			n.accommodations = append(n.accommodations, why)
		}
		argv := c.Argv
		if c.Shell {
			argv = []string{"sh", "-c", c.Argv[0]}
		}
		if _, err := n.Exec(ctx, argv...); err != nil {
			return fmt.Errorf("%s: runcmd %q: %w", n.Name(), c, err)
		}
	}
	return nil
}

// Accommodations are the changes this rig made to the last document
// it applied, because a container cannot satisfy what the document
// asked for.
//
// Reported rather than silent. The shell harness rewrote the rendered
// kubeadm join with a string substitution and said nothing, so every
// kubeadm row ran a command the product had not rendered and no
// reader could tell.
func (n *Node) Accommodations() []string { return n.accommodations }

// accommodate adjusts one command for what a container cannot do, and
// says why.
//
// Only the container rig does this, and that is the point: a virtual
// machine has a kernel of its own, so it runs the rendered command
// unchanged and preflight means something there. Anything this
// function has to touch is a fidelity gap the VM tier closes.
func accommodate(c cloudinit.Cmd) (cloudinit.Cmd, string) {
	const join = "kubeadm join"
	const relax = " --ignore-preflight-errors=all"

	for i, word := range c.Argv {
		if !strings.Contains(word, join) || strings.Contains(word, "--ignore-preflight-errors") {
			continue
		}
		// kubeadm's preflight inspects the kernel it runs on, which
		// in a container is this host's: it cannot load the "configs"
		// module, cannot read what it needs from /proc/sys, and
		// objects to things the node neither owns nor can change.
		out := c
		out.Argv = append([]string(nil), c.Argv...)
		out.Argv[i] = strings.Replace(word, join, join+relax, 1)
		return out, "relaxed kubeadm's preflight: a container shares this host's kernel and cannot satisfy it"
	}
	return c, ""
}

// Skipped reports the cloud-config keys this rig would not apply for
// the given document, so a caller can record the difference between
// what the pattern says and what the row proves.
func Skipped(cloudConfig []byte) ([]string, error) {
	doc, err := cloudinit.Parse(cloudConfig)
	if err != nil {
		return nil, err
	}
	return doc.Skipped, nil
}

// shellQuote wraps a word for the one place this package uses a
// shell: a redirect, which has no argv form.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
