// Package vm runs cluster nodes as virtual machines.
//
// The authoritative rig, and the only one that reaches a boot. Each
// node has a kernel of its own, a bootloader, an init, and a real
// cloud-init that reads the userdata the product rendered — none of
// which a container can offer, and all of which the product depends
// on in production.
//
// Lab commands use QEMU Guest Agent over virtio-serial, independent of the
// guest network. SSH is used only while preparing the platform image.
package vm

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Guest is the SSH account used only for platform image preparation.
const Guest = "root"

// KeyPath is where each wrapper keeps the key for its own guest.
const KeyPath = "/tmp/cldt-guest-key"

// ControlPath is where a wrapper keeps the shared connection to its
// own guest. %C is a hash of the destination, so a wrapper that
// reaches one guest keeps one socket.
const ControlPath = "/tmp/cldt-ssh-%C"

// Runner executes a command on this host.
type Runner func(ctx context.Context, stdin io.Reader, argv ...string) (stdout, stderr []byte, code int, err error)

// Node is one machine.
type Node struct {
	node    lab.Node
	labName string
	run     Runner
	// rig is what launches this machine. A node can be replaced, and
	// replacing one needs the lab it belongs to.
	rig *Rig
}

// Wrapper is the container the machine runs inside.
func (n *Node) Wrapper() string { return "clab-" + n.labName + "-" + n.node.Name }

// Name is the topology's name for the node.
func (n *Node) Name() string { return n.node.Name }

// ssh reaches the image builder through the original wrapper's loopback NAT.
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
		// One connection per guest, shared by every command.
		//
		// A guest's sshd admits ten unauthenticated connections at
		// once and drops the rest: the reachability matrix opens one
		// ssh per check and fans out over every pair, so a row on
		// machines walks straight into it. Measured in the guest's own
		// log — "beginning MaxStartups throttling", "drop connection
		// #11 ... past MaxStartups" — while the check it killed
		// reported ssh's exit 255 against a crictl that never ran, and
		// the row failed one of a hundred and forty for a reason that
		// had nothing to do with the datapath.
		//
		// Multiplexing also removes a TCP handshake and a key exchange
		// from every command, which on this rig is every command the
		// harness runs.
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+ControlPath,
		"-o", "ControlPersist=120s",
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
	quoted := make([]string, 0, len(argv))
	for _, word := range argv {
		quoted = append(quoted, shellQuote(word))
	}
	return append(full, strings.Join(quoted, " "))
}

func (n *Node) Exec(ctx context.Context, argv ...string) ([]byte, error) {
	return n.Pipe(ctx, nil, argv...)
}

func (n *Node) Pipe(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	full := []string{"docker", "exec"}
	input := "0"
	if stdin != nil {
		full = append(full, "-i")
		input = "1"
	}
	timeout := 2 * time.Minute
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	full = append(full, n.Wrapper(), "/cldt-guest", "exec", timeout.String(), input)
	if n.rig != nil {
		full = append(full, n.rig.GuestExecPrefix...)
	}
	full = append(full, argv...)
	if n.rig != nil && n.rig.builderSSH {
		full = n.ssh(stdin != nil, argv...)
	}
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

// Cut takes the guest's sole Ethernet link down and leaves it running.
// Execution remains available over the non-network virtio-serial channel.
func (n *Node) Cut(ctx context.Context) error {
	for _, i := range n.dataInterfaces() {
		if _, err := n.Exec(ctx, "ip", "link", "set", i, "down"); err != nil {
			return err
		}
	}
	return nil
}

// Restore reverses Cut. The guest's distribution and CNI own addressing;
// OVN, for example, moves the host address from ens2 onto br-ex. Reassigning
// the topology address here would change the datapath being tested.
func (n *Node) Restore(ctx context.Context) error {
	for _, i := range n.dataInterfaces() {
		if _, err := n.Exec(ctx, "ip", "link", "set", i, "up"); err != nil {
			return err
		}
	}
	return nil
}

// Kill stops the machine ungracefully: the wrapper goes, and qemu
// with it. No shutdown, no goodbye, which is what a power event is.
func (n *Node) Kill(ctx context.Context) error {
	state, _, stateCode, stateErr := n.run(ctx, nil, "docker", "inspect", "--format", "{{.State.Running}}", n.Wrapper())
	if stateErr == nil && stateCode == 0 && strings.TrimSpace(string(state)) == "false" {
		return nil
	}

	_, errb, code, err := n.run(ctx, nil, "docker", "kill", n.Wrapper())
	if err != nil {
		return err
	}
	if code != 0 {
		return &rig.ExitError{Node: n.Name(), Argv: []string{"docker", "kill"}, Code: code, Stderr: errb}
	}
	return nil
}

// Boot starts the wrapper and reconnects its sole data NIC.
func (n *Node) Boot(ctx context.Context) error {
	_, errb, code, err := n.run(ctx, nil, "docker", "start", n.Wrapper())
	if err != nil {
		return err
	}
	if code != 0 {
		return &rig.ExitError{Node: n.Name(), Argv: []string{"docker", "start"}, Code: code, Stderr: errb}
	}
	return n.plumb(ctx)
}

// plumb gives the wrapper every link the topology says it has.
func (n *Node) plumb(ctx context.Context) error {
	for _, l := range n.links() {
		// A stale host-side veth outlives the namespace it belonged
		// to, and a new one cannot take a name that already exists.
		_, _, _, _ = n.run(ctx, nil, "sudo", "ip", "link", "del", l.endpoint)
		if _, errb, code, err := n.run(ctx, nil, "sudo", "containerlab", "tools", "veth", "create",
			"-a", n.Wrapper()+":"+l.wrapperInterface,
			"-b", "bridge:"+l.bridge+":"+l.endpoint); err != nil {
			return fmt.Errorf("re-plumbing %s %s: %w", n.Name(), l.wrapperInterface, err)
		} else if code != 0 {
			return fmt.Errorf("re-plumbing %s %s: %s", n.Name(), l.wrapperInterface, errb)
		}
	}
	return nil
}

// link is one of the wrapper's connections, as the topology declares
// it.
type link struct {
	wrapperInterface string
	bridge           string
	endpoint         string
}

// links are exactly the connections declared by the topology.
func (n *Node) links() []link {
	var out []link
	for _, i := range n.node.Interfaces {
		out = append(out, link{
			wrapperInterface: i.Name,
			bridge:           i.Segment,
			endpoint:         lab.EndpointName(i.Segment, n.node),
		})
	}
	return out
}

// Userdata launches the machine with this userdata.
//
// A machine reads its userdata once, at its first boot, from the seed
// its platform gave it. Handing it to an instance that is already
// running is not something a platform does — and interpreting it
// here, which is all the container rig can do, is the thing this rig
// exists to stop.
//
// What a platform does is launch an instance with it, so that is what
// this does. The bytes go into the node's seed, the instance is taken
// away, and one is created in its place: the new wrapper builds a
// fresh cloud-init ISO from the seed and qemu gets a fresh overlay
// disk, so the guest boots for the first time and its own cloud-init
// reads the userdata the product rendered. The instance has to be
// replaced rather than restarted because vrnetlab creates the overlay
// only when none exists, and a restart keeps the container's writable
// layer — the guest would come back as the same instance, past its
// first boot, having read nothing.
func (n *Node) Userdata(ctx context.Context, cloudConfig []byte) error {
	if n.rig == nil {
		return fmt.Errorf("%s: no lab to launch into", n.Name())
	}
	if err := n.rig.Seed(n.node.Name, cloudConfig); err != nil {
		return fmt.Errorf("seeding %s: %w", n.Name(), err)
	}
	// Request a fresh disk at the next wrapper start, after QEMU has stopped.
	marker := filepath.Join(n.rig.SeedDir(n.Name()), "reset-instance")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		return err
	}
	if _, errb, code, err := n.run(ctx, nil, "docker", "cp", marker, n.Wrapper()+":/cldt-reset-instance"); err != nil {
		return fmt.Errorf("resetting %s: %w", n.Name(), err)
	} else if code != 0 {
		return fmt.Errorf("resetting %s: %s", n.Name(), errb)
	}
	if err := n.Kill(ctx); err != nil {
		return fmt.Errorf("stopping %s: %w", n.Name(), err)
	}
	if err := n.Boot(ctx); err != nil {
		return fmt.Errorf("launching %s: %w", n.Name(), err)
	}
	return n.rig.waitForNode(ctx, n.node.Name, BootTimeout)
}

// BootstrapFailure observes the Ubuntu VM image's cloud-init through serial.
func (n *Node) BootstrapFailure(ctx context.Context) error {
	return rig.CloudInitFailure(ctx, n)
}

func (n *Node) Bootstrap(ctx context.Context, data rig.BootstrapData) error {
	if data.Format != rig.CloudConfig {
		return fmt.Errorf("%s: Ubuntu VM requires cloud-config, got %q", n.Name(), data.Format)
	}
	return n.Userdata(ctx, data.Value)
}

// Interface is what the guest calls the lab's nth link.
func (n *Node) Interface(nth int) string {
	if nth < 0 || nth >= len(n.node.Interfaces) {
		return ""
	}
	return GuestInterface(nth)
}

// dataInterfaces translates topology link positions to guest PCI names.
func (n *Node) dataInterfaces() []string {
	out := make([]string, 0, len(n.node.Interfaces))
	for i := range n.node.Interfaces {
		out = append(out, GuestInterface(i))
	}
	return out
}

// GuestInterface names the data NIC by PCI slot. Observed on the single-NIC
// guest: lo and ens2 (alternative name enp1s2).
func GuestInterface(n int) string { return "ens" + strconv.Itoa(n+2) }

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (n *Node) BootstrapComplete(ctx context.Context) (bool, error) {
	return rig.CloudInitComplete(ctx, n)
}
