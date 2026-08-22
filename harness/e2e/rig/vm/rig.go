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
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
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

func (r *Rig) Node(name string) rig.Node {
	return &Node{node: r.Topology.MustNode(name), labName: r.Topology.Name, run: r.runner()}
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
		// The login this harness reaches the machine by, installed by
		// the guest's own cloud-init on first boot.
		setup := fmt.Sprintf(`#!/bin/sh
mkdir -p /home/%[1]s/.ssh
printf '%%s\n' %[2]q >> /home/%[1]s/.ssh/authorized_keys
chown -R %[1]s:%[1]s /home/%[1]s/.ssh
chmod 700 /home/%[1]s/.ssh
chmod 600 /home/%[1]s/.ssh/authorized_keys
`, Guest, strings.TrimSpace(string(pub)))
		if err := os.WriteFile(filepath.Join(dir, "extra-setup.sh"), []byte(setup), 0o755); err != nil {
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

	// Each wrapper gets the key for its own guest, because that is
	// where the command that uses it runs.
	key, err := os.ReadFile(r.privateKeyPath())
	if err != nil {
		return err
	}
	for _, n := range r.Topology.Nodes {
		if !n.IsClusterNode() {
			continue
		}
		wrapper := "clab-" + r.Topology.Name + "-" + n.Name
		if _, _, _, err := r.runner()(ctx, bytes.NewReader(key),
			"docker", "exec", "-i", wrapper, "sh", "-c", "cat > "+KeyPath+" && chmod 600 "+KeyPath); err != nil {
			return fmt.Errorf("placing the key in %s: %w", wrapper, err)
		}
	}
	return nil
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
