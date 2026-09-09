// Command nodeimage bakes a Kubernetes node image for the machine
// rig.
//
// kubeadm configures a node; it does not make one. The site's nodes
// can be built in place, because the harness reaches them before
// anything asks them to be a node — but a remote cannot be. A remote
// is launched as a new instance precisely so its own cloud-init reads
// the document the product rendered, and that document runs
// "kubeadm join" straight away, on a disk that has existed for
// seconds.
//
// That is not a gap in the product. It is what a kubeadm deployment
// assumes: you build an image with the runtime and the tools on it,
// and instances come up ready to join. So the platform builds one.
//
// The image is made the way the site's nodes are, by the same code,
// so the two cannot drift: boot a guest, provision it, clean the
// marks that make cloud-init treat a boot as a repeat, shut it down,
// and flatten the result into a standalone disk.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster/kubeadm"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/vm"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

// LabName and NodeName are what the builder's wrapper is called, and
// they are not arbitrary: the VM rig addresses a guest through
// "clab-<lab>-<node>", so these make an ordinary rig node out of a
// container this command started itself.
const (
	LabName  = "nodeimage"
	NodeName = "builder"
	Wrapper  = "clab-" + LabName + "-" + NodeName
)

func main() {
	var (
		workDir      = flag.String("work-dir", "_workvm", "where the artifacts and the finished image are kept")
		out          = flag.String("out", "", "the image to write (default <work-dir>/kubeadm-node.qcow2)")
		keep         = flag.Bool("keep", false, "leave the builder running for inspection")
		platformOnly = flag.Bool("platform-only", false, "prepare the guest-agent platform image without Kubernetes")
		diskGiB      = flag.Int("disk-gib", 32, "minimum exported disk capacity in GiB (20–2048); larger source disks are preserved")
	)
	flag.Parse()
	if err := validateDiskGiB(*diskGiB); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if *out == "" {
		*out = *workDir + "/kubeadm-node.qcow2"
		if *platformOnly {
			*out = *workDir + "/platform-node.qcow2"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	if err := build(ctx, *workDir, *out, *keep, *platformOnly, *diskGiB); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	if *platformOnly {
		fmt.Printf("\n  %s is a platform image with qemu-guest-agent\n", *out)
		return
	}
	fmt.Printf("\n  %s is a node image: containerd %s, runc %s, Kubernetes %s\n",
		*out, kubeadm.ContainerdVersion, kubeadm.RuncVersion, kubeadm.KubernetesVersion)
}

func build(ctx context.Context, workDir, out string, keep, platformOnly bool, diskGiB int) error {
	if err := validateDiskGiB(diskGiB); err != nil {
		return err
	}
	rig := vm.New(builderTopology(), workDir)

	step("starting a guest")
	if err := rig.StartBuilder(ctx, Wrapper); err != nil {
		return err
	}
	if !keep {
		defer func() { _ = run(context.Background(), "docker", "rm", "-f", Wrapper) }()
	}

	node := rig.Node(NodeName)
	step("waiting for it to boot")
	if err := wait.Until(ctx, 12*time.Minute, "the guest never became reachable",
		func(ctx context.Context) error {
			_, err := node.Exec(ctx, "true")
			return err
		}); err != nil {
		return err
	}

	step("preparing the platform's non-network management agent")
	if _, err := node.Exec(ctx, "sh", "-ec", "apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y qemu-guest-agent && systemctl enable qemu-guest-agent"); err != nil {
		return err
	}
	if !platformOnly {
		step("building it into a Kubernetes node")
		if err := kubeadm.EnsureStack(ctx, rig, workDir, []string{NodeName}); err != nil {
			return err
		}
	}

	// Everything that would make the next boot a repeat rather than a
	// first one. cloud-init records the instance it configured and
	// skips a boot it has already seen; a machine-id carried in an
	// image gives every instance made from it the same identity, which
	// DHCP and systemd both key on.
	step("making the next boot a first boot")
	if _, err := node.Exec(ctx, "sh", "-c",
		"cloud-init clean --logs --seed >/dev/null 2>&1; "+
			"rm -f /etc/cloud/cloud-init.disabled; "+
			"truncate -s 0 /etc/machine-id; "+
			"rm -f /var/lib/dbus/machine-id; "+
			"rm -rf /var/lib/rancher /var/lib/k0s /etc/rancher /etc/k0s; "+
			"rm -f /root/.ssh/authorized_keys; "+
			"sync"); err != nil {
		return fmt.Errorf("clearing the guest's first-boot marks: %w", err)
	}

	// Cleanly, so the filesystem in the image is one a machine would
	// find rather than one it has to repair.
	step("shutting it down")
	_, _ = node.Exec(ctx, "sh", "-c", "nohup sh -c 'sleep 1; poweroff' >/dev/null 2>&1 &")
	if err := waitForShutdown(ctx, 5*time.Minute); err != nil {
		return err
	}

	// Flattened rather than left as an overlay: an image with a
	// backing file is only usable where that file is, and this one is
	// bound into a container that has never seen it.
	step("flattening it into a standalone disk")
	if err := run(ctx, "docker", "exec", Wrapper, "qemu-img", "convert",
		"-O", "qcow2", "/jammy-ubuntu-cloud-overlay.qcow2", "/nodeimage.qcow2"); err != nil {
		return fmt.Errorf("flattening the disk: %w", err)
	}
	// Only grow the clean, standalone export. Cloud-init grows the guest's
	// partition and filesystem at the next first boot; retained VM disks are
	// never resized by this image-building operation.
	if err := growImage(ctx, "/nodeimage.qcow2", diskGiB, func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", append([]string{"exec", Wrapper, "qemu-img"}, args...)...).Output()
	}); err != nil {
		return fmt.Errorf("sizing the exported disk: %w", err)
	}
	if err := run(ctx, "docker", "cp", Wrapper+":/nodeimage.qcow2", out); err != nil {
		return fmt.Errorf("copying the image out: %w", err)
	}
	return nil
}

// waitForShutdown waits for qemu to stop, which is what tells us the
// guest actually finished writing.
func waitForShutdown(ctx context.Context, within time.Duration) error {
	return wait.Until(ctx, within, "the guest never shut down", func(ctx context.Context) error {
		out, err := exec.CommandContext(ctx, "docker", "exec", Wrapper,
			"sh", "-c", "pgrep -c qemu-system || true").Output()
		if err != nil {
			// The wrapper exits with its guest, which also means the
			// guest is down.
			return nil
		}
		if strings.TrimSpace(string(out)) != "0" {
			return fmt.Errorf("qemu is still running")
		}
		return nil
	})
}

// builderTopology is one machine and nothing else: this command needs
// a node it can reach, not a lab.
func builderTopology() lab.Topology {
	return lab.Topology{
		Name: LabName,
		Nodes: []lab.Node{{
			Name: NodeName,
			Role: lab.Worker,
			Interfaces: []lab.Interface{{
				Name: "eth1", Segment: lab.LANSegment, Address: "10.10.0.99/24",
			}},
		}},
	}
}

func step(what string) { fmt.Printf("--- %s ---\n", what) }

func run(ctx context.Context, argv ...string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %w: %s", argv, err, out)
	}
	return nil
}
