package vm

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/container"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

var (
	_ rig.Rig  = (*Rig)(nil)
	_ rig.Node = (*Node)(nil)
)

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

// Rig is the lab, with its cluster nodes as machines.
type Rig struct {
	Topology lab.Topology
	WorkDir  string
	Run      Runner
	// GuestExecPrefix selects a platform's serial execution policy wrapper.
	GuestExecPrefix []string
	builderSSH      bool
}

// New returns a VM rig.
func New(topo lab.Topology, workDir string) *Rig {
	return &Rig{Topology: topo, WorkDir: workDir, Run: Exec}
}

func (r *Rig) Kind() string { return lab.VM.String() }

func (r *Rig) runner() Runner {
	if r.Run == nil {
		return Exec
	}
	return r.Run
}

// Node returns the node, which is a machine only if it runs a
// kubelet.
//
// The routers, the cloud edges and the bastion stay containers under
// this rig, as the topology says: an appliance with no kubelet gains
// nothing from a kernel of its own. They are also reached the way
// containers are — there is no guest inside them to ssh to, and
// trying produced a command not found on the first bring-up.
func (r *Rig) Node(name string) rig.Node {
	n := r.Topology.MustNode(name)
	if !n.IsClusterNode() {
		return r.appliances().Node(name)
	}
	return &Node{node: n, labName: r.Topology.Name, run: r.runner(), rig: r}
}

// appliances reaches the containers this rig still has.
func (r *Rig) appliances() *container.Rig {
	return &container.Rig{Topology: r.Topology, WorkDir: r.WorkDir, Run: container.Runner(r.runner())}
}

// SeedDir is where a machine's first-boot material lives on this
// host, bound into its wrapper.
func (r *Rig) SeedDir(node string) string {
	return filepath.Join(r.WorkDir, "seed", node)
}

// TopologyPath is where the generated containerlab file is written.
func (r *Rig) TopologyPath() string {
	return filepath.Join(r.WorkDir, r.Topology.Name+".vm.clab.yml")
}

// Seed places the userdata a machine will read at its first boot.
//
// Before it boots, because that is when a machine reads it: a
// platform hands an instance its userdata as it launches, and
// cloud-init is the thing that reads it. Writing it afterwards would
// mean something other than cloud-init applied it, which is the whole
// difference this rig exists to remove.
func (r *Rig) Seed(node string, cloudConfig []byte) error {
	dir := r.SeedDir(node)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "extra-userdata.yaml"), cloudConfig, 0o600)
}

// Up writes platform metadata and deploys the single-NIC guests.
func (r *Rig) Up(ctx context.Context) error {
	for _, n := range r.Topology.Nodes {
		if !n.IsClusterNode() {
			continue
		}
		dir := r.SeedDir(n.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		// An empty metadata file makes vrnetlab emit the cloud-init instance ID.
		// Lab execution needs no SSH credentials.
		if err := os.WriteFile(filepath.Join(dir, "extra-authorized-keys"), nil, 0600); err != nil {
			return err
		}
		// Kept because the wrapper binds it, and because a machine
		// may yet need first-boot setup that is the platform's rather
		// than the tenant's.
		if err := os.WriteFile(filepath.Join(dir, "extra-setup.sh"), []byte("#!/bin/sh\n:\n"), 0o755); err != nil {
			return err
		}

		// The platform's network configuration, applied by the
		// guest's cloud-init before any userdata runs — so a machine
		// whose bootstrap dials the site has a segment to dial from.
		netcfg, err := NetworkConfig(n)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "extra-network.yaml"), []byte(netcfg), 0o644); err != nil {
			return err
		}

		// An empty document by default: a machine with nothing to do
		// at first boot still needs the file to exist, because its
		// wrapper binds it.
		userdata := filepath.Join(dir, "extra-userdata.yaml")
		if _, err := os.Stat(userdata); os.IsNotExist(err) {
			if err := os.WriteFile(userdata, []byte("#cloud-config\n{}\n"), 0o644); err != nil {
				return err
			}
		}
	}

	yaml, err := r.Topology.ContainerlabYAML(lab.VM)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.WorkDir, 0o755); err != nil {
		return err
	}
	if err := r.DestroyDeployed(ctx); err != nil {
		return err
	}
	if err := os.WriteFile(r.TopologyPath(), []byte(yaml), 0o644); err != nil {
		return err
	}
	if _, errb, code, err := r.runner()(ctx, nil,
		"sudo", "containerlab", "deploy", "-t", r.TopologyPath(), "--reconfigure"); err != nil || code != 0 {
		if err != nil {
			return fmt.Errorf("deploying: %w", err)
		}
		return fmt.Errorf("deploying: containerlab exited %d: %s", code, errb)
	}

	return r.WaitReady(ctx, BootTimeout)
}

// placeKey puts the guest's key in its own wrapper, which is where
// the command that uses it runs. A wrapper that was replaced has a
// fresh filesystem and needs one again.
func (r *Rig) placeKey(ctx context.Context, node string) error {
	key, err := os.ReadFile(r.privateKeyPath())
	if err != nil {
		return err
	}
	wrapper := "clab-" + r.Topology.Name + "-" + node
	if _, _, _, err := r.runner()(ctx, bytes.NewReader(key),
		"docker", "exec", "-i", wrapper, "sh", "-c", "cat > "+KeyPath+" && chmod 600 "+KeyPath); err != nil {
		return fmt.Errorf("placing the key in %s: %w", wrapper, err)
	}
	return nil
}

// BootTimeout is how long a machine has to become reachable.
//
// Generous, because this is a boot: firmware, a bootloader, a kernel,
// an init, and a cloud-init that has to run before there is a login
// to use. A container is ready when it starts and needed none of
// this, which is why nothing waited until there were machines.
const BootTimeout = 12 * time.Minute

// WaitReady blocks until every machine answers.
//
// A cloud does not hand out an instance that cannot be reached, and a
// harness that configured one before it had booted would report the
// machine broken for the time it was still starting: the first VM
// bring-up failed on "Connection timed out during banner exchange",
// which is sshd not being up yet and reads like a network fault.
func (r *Rig) WaitReady(ctx context.Context, within time.Duration) error {
	for _, n := range r.Topology.Nodes {
		if !n.IsClusterNode() {
			continue
		}
		if err := r.waitForNode(ctx, n.Name, within); err != nil {
			return err
		}
	}
	return nil
}

// waitForNode blocks until one machine answers, or until it is clear
// that it will not.
func (r *Rig) waitForNode(ctx context.Context, node string, within time.Duration) error {
	return wait.Until(ctx, within, node+" did not become reachable after being started",
		func(ctx context.Context) error {
			if _, err := r.Node(node).Exec(ctx, "true"); err != nil {
				if crash := r.crashing(ctx, node); crash != nil {
					return wait.Fatal(crash)
				}
				return err
			}
			return nil
		})
}

// RestartsMeaningCrashLoop is how many restarts distinguish a wrapper
// failing to start from one that has merely been restarted once.
const RestartsMeaningCrashLoop = 2

// crashing reports the wrapper failing to start at all, rather than a
// guest that has not finished booting.
//
// The two look identical from outside — nothing answers either way —
// and only one of them is worth waiting for. A wrapper whose launcher
// rejects its own configuration exits, its supervisor starts it
// again, and it exits again; waiting the full boot timeout on that
// spends twelve minutes to report a machine as slow when what
// happened is that it never ran. The launcher's own output says which
// it is, so it is carried out with the failure rather than left for
// whoever goes looking.
func (r *Rig) crashing(ctx context.Context, node string) error {
	wrapper := "clab-" + r.Topology.Name + "-" + node
	out, _, code, err := r.runner()(ctx, nil, "docker", "inspect",
		"--format", "{{.State.Status}} {{.RestartCount}}", wrapper)
	if err != nil || code != 0 {
		return nil
	}
	var status string
	var restarts int
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &status, &restarts); err != nil {
		return nil
	}
	if status != "restarting" || restarts < RestartsMeaningCrashLoop {
		return nil
	}
	logs, _, _, _ := r.runner()(ctx, nil, "docker", "logs", "--tail", "20", wrapper)
	return fmt.Errorf("%s's wrapper has failed to start %d times, so no machine is booting:\n%s",
		node, restarts, strings.TrimSpace(string(logs)))
}

// Down destroys the lab.
func (r *Rig) Down(ctx context.Context) error {
	_, errb, code, err := r.runner()(ctx, nil,
		"sudo", "containerlab", "destroy", "-t", r.TopologyPath(), "--cleanup")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("destroying: containerlab exited %d: %s", code, errb)
	}
	return nil
}

func (r *Rig) privateKeyPath() string { return filepath.Join(r.WorkDir, "guest-key") }
func (r *Rig) publicKeyPath() string  { return filepath.Join(r.WorkDir, "guest-key.pub") }

// ensureKey makes one keypair for the lab, once.
func (r *Rig) ensureKey() error {
	if _, err := os.Stat(r.privateKeyPath()); err == nil {
		return nil
	}
	if err := os.MkdirAll(r.WorkDir, 0o755); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return err
	}
	if err := os.WriteFile(r.privateKeyPath(), pem.EncodeToMemory(block), 0o600); err != nil {
		return err
	}
	signer, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	return os.WriteFile(r.publicKeyPath(), ssh.MarshalAuthorizedKey(signer), 0o644)
}

// StartBuilder runs a single guest outside any lab, for making an
// image from.
//
// Outside a lab deliberately: it wants a route out and nothing else,
// so it takes docker's own network rather than the segments this
// topology models, and the launcher's default addressing rather than
// the platform's. Nothing about the lab is being tested here — the
// guest exists to be provisioned and then flattened into a disk.
func (r *Rig) StartBuilder(ctx context.Context, wrapper string) error {
	r.builderSSH = true
	if err := r.ensureKey(); err != nil {
		return err
	}
	pub, err := os.ReadFile(r.publicKeyPath())
	if err != nil {
		return err
	}
	dir := r.SeedDir(builderSeed)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "extra-authorized-keys"), pub, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "extra-userdata.yaml"), []byte("#cloud-config\n{}\n"), 0o644); err != nil {
		return err
	}

	_, _, _, _ = r.runner()(ctx, nil, "docker", "rm", "-f", wrapper)

	seed, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	_, errb, code, err := r.runner()(ctx, nil, "docker", "run", "-d", "--name", wrapper,
		"--privileged",
		"-e", "QEMU_MEMORY="+strconv.Itoa(lab.VMMemoryMB),
		"-e", "QEMU_SMP="+strconv.Itoa(lab.VMCPUs),
		"-e", "CLAB_MGMT_INTF=eth0",
		"-e", "VR_MGMT_IS_A_LINK=1",
		"-v", seed+"/extra-authorized-keys:/extra-authorized-keys:ro",
		"-v", seed+"/extra-userdata.yaml:/extra-userdata.yaml:ro",
		"vrnetlab/canonical_ubuntu:jammy", "--hostname", builderSeed)
	if err != nil {
		return fmt.Errorf("starting the builder: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("starting the builder: %s", errb)
	}
	return r.placeKey(ctx, strings.TrimPrefix(wrapper, "clab-"+r.Topology.Name+"-"))
}

// builderSeed is the name the image builder's guest goes by.
const builderSeed = "builder"
