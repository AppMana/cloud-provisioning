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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "extra-userdata.yaml"), cloudConfig, 0o644)
}

// Up generates a key, writes each machine's first-boot material, and
// deploys.
func (r *Rig) Up(ctx context.Context) error {
	if err := r.ensureKey(); err != nil {
		return err
	}
	pub, err := os.ReadFile(r.publicKeyPath())
	if err != nil {
		return err
	}
	for _, n := range r.Topology.Nodes {
		if !n.IsClusterNode() {
			continue
		}
		dir := r.SeedDir(n.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		// The login this harness reaches the machine by, handed to
		// the instance as metadata.
		//
		// Metadata rather than a script in the user-data, because
		// cloud-init merges a multipart user-data by replacing lists:
		// a key installed from a runcmd is lost the moment the
		// document under test carries a runcmd of its own, and the
		// machine then boots, answers on sshd, and denies every
		// login. A cloud hands the instance its keypair through the
		// datasource for exactly this reason — what the tenant puts
		// in user-data cannot take away the operator's way in.
		if err := os.WriteFile(filepath.Join(dir, "extra-authorized-keys"), pub, 0o644); err != nil {
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

	// Each machine's own bridge, before anything is deployed onto it.
	//
	// containerlab attaches to bridges that already exist and refuses
	// a topology naming one that does not. These carry no address: a
	// machine is reached through its wrapper, on the wrapper's own
	// loopback, so nothing needs to be routable here — the bridge
	// exists so that the machine has a management link of its own
	// rather than one shared with every other node in the lab.
	for _, n := range r.Topology.Nodes {
		if !n.IsClusterNode() {
			continue
		}
		bridge := lab.ManagementBridge(n.Name)
		_, _, _, _ = r.runner()(ctx, nil, "sudo", "ip", "link", "add", "name", bridge, "type", "bridge")
		if _, errb, code, err := r.runner()(ctx, nil, "sudo", "ip", "link", "set", bridge, "up"); err != nil || code != 0 {
			if err != nil {
				return fmt.Errorf("raising %s: %w", bridge, err)
			}
			return fmt.Errorf("raising %s: %s", bridge, errb)
		}
	}

	yaml, err := r.Topology.ContainerlabYAML(lab.VM)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.WorkDir, 0o755); err != nil {
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

	for _, n := range r.Topology.Nodes {
		if !n.IsClusterNode() {
			continue
		}
		if err := r.placeKey(ctx, n.Name); err != nil {
			return err
		}
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

// waitForNode blocks until one machine answers.
func (r *Rig) waitForNode(ctx context.Context, node string, within time.Duration) error {
	return wait.Until(ctx, within, node+" did not become reachable after being started",
		func(ctx context.Context) error {
			_, err := r.Node(node).Exec(ctx, "true")
			return err
		})
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
