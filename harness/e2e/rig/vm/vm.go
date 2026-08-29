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

// Guest is the account this harness reaches a machine by.
//
// root, because that is who cloud-init gives the keys that arrive as
// instance metadata. Its ssh module asks for a user marked default
// and, finding none, writes them to root alone — measured on a guest,
// in cloud-init's own log: "Writing to /root/.ssh/authorized_keys
// [600] 81 bytes", and nothing written to the unprivileged account
// the launcher creates. Marking that account default does not change
// it.
//
// This is also what the access is. The harness is the platform, and a
// platform's way into an instance is out of band — over a management
// path no segment of the lab carries — not an ordinary login that
// escalates. Saying so removes the sudo every command used to be
// wrapped in, and with it a quoting seam that had already produced
// one failure.
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

// Boot brings the machine back: its links, then its guest.
//
// A restarted container gets a new network namespace, so every link
// containerlab made into the old one is gone — and on this rig that
// is all of them, the management link included, because the wrapper
// runs with no network of its own. The launcher waits for the
// management interface to appear before it starts qemu at all, so a
// machine booted without its links does not come back slowly, it
// hangs.
//
// Restoring them is the platform handing a machine its NICs back, and
// it belongs here rather than in a generic replumb because only this
// rig knows a machine has a management link to restore.
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

// links are the management link first, then the lab's own, in the
// order the topology writes them — which is the order qemu attaches
// them and so the order the guest names them.
func (n *Node) links() []link {
	out := []link{{
		wrapperInterface: lab.ManagementInterfaceName,
		bridge:           lab.ManagementBridge(n.node.Name),
		endpoint:         "m-" + n.node.Name,
	}}
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
	// The instance's disk goes, and with it everything the last one
	// ever did.
	//
	// vrnetlab creates the overlay only when none exists and disables
	// cloud-init after a first boot, so a guest whose disk survives
	// comes back as the same instance having read nothing — the run
	// would report a bootstrap that succeeded and a node that never
	// joined. Unlinking it while qemu still holds it open is safe:
	// the process keeps writing to an inode with no name until it
	// exits, and what starts next finds nothing there.
	//
	// The wrapper itself stays. It is the chassis, not the instance,
	// and containerlab will not put a node back into a lab it has
	// already deployed — with or without a node filter, it refuses.
	if _, errb, code, err := n.run(ctx, nil, "docker", "exec", n.Wrapper(),
		"sh", "-c", "rm -f /*-overlay.qcow2"); err != nil {
		return fmt.Errorf("replacing %s's disk: %w", n.Name(), err)
	} else if code != 0 {
		return fmt.Errorf("replacing %s's disk: %s", n.Name(), errb)
	}
	if err := n.Kill(ctx); err != nil {
		return fmt.Errorf("stopping %s: %w", n.Name(), err)
	}
	if err := n.Boot(ctx); err != nil {
		return fmt.Errorf("launching %s: %w", n.Name(), err)
	}
	if err := n.rig.placeKey(ctx, n.node.Name); err != nil {
		return err
	}
	return n.rig.waitForNode(ctx, n.node.Name, BootTimeout)
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
