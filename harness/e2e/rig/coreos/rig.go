// Package coreos runs SCOS guests using the same topology and serial transport
// as the Ubuntu VM rig. Site controllers boot the official agent-installer ISO;
// remote instances consume product-owned Ignition at first boot.
package coreos

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/vm"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/vm/scos"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
	clab "github.com/appmana/labcontainers/pkg/containerlab"
	"github.com/srl-labs/containerlab/core"
	"github.com/srl-labs/containerlab/types"
)

const Image = "cloud-provisioning/scos:single-nic"

type Rig struct {
	*vm.Rig
	AgentISO string
	BaseDisk string
}

func New(t lab.Topology, workDir, iso, disk string) *Rig {
	base := vm.New(t, workDir)
	base.GuestExecPrefix = []string{scos.ExecWrapper}
	return &Rig{Rig: base, AgentISO: iso, BaseDisk: disk}
}

func MAC(name string) string {
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", sum[0], sum[1], sum[2])
}

func (r *Rig) stateDir(name string) string { return filepath.Join(r.WorkDir, "nodes", name) }

func (r *Rig) Node(name string) rig.Node {
	n := r.Rig.Node(name)
	if !r.Topology.MustNode(name).IsClusterNode() {
		return n
	}
	return &node{Node: n, rig: r}
}

// TopologyYAML is the legacy CLI serialization boundary.
func (r *Rig) TopologyYAML() ([]byte, error) {
	config, err := r.TopologyConfig()
	if err != nil {
		return nil, err
	}
	source, err := clab.Source(config)
	if err != nil {
		return nil, err
	}
	return source.GetYaml(), nil
}

// TopologyConfig specializes native objects with the platform's boot media.
// There is no YAML round trip or hand-maintained schema of node properties.
func (r *Rig) TopologyConfig() (*core.Config, error) {
	config, err := r.Topology.ContainerlabConfig(lab.Container)
	if err != nil {
		return nil, err
	}
	for _, n := range r.Topology.Nodes {
		if !n.IsClusterNode() {
			continue
		}
		state, err := filepath.Abs(r.stateDir(n.Name))
		if err != nil {
			return nil, err
		}
		seed, err := filepath.Abs(r.SeedDir(n.Name))
		if err != nil {
			return nil, err
		}
		binds := []string{state + ":/state", seed + ":/seed:ro"}
		memory := "8192"
		if n.Role == lab.ControlPlane {
			iso, err := filepath.Abs(r.AgentISO)
			if err != nil {
				return nil, err
			}
			binds = append(binds, iso+":/agent.iso:ro")
			memory = "16384"
		} else {
			disk, err := filepath.Abs(r.BaseDisk)
			if err != nil {
				return nil, err
			}
			binds = append(binds, disk+":/scos-base.qcow2:ro")
		}
		config.Topology.Nodes[n.Name] = &types.NodeDefinition{Kind: "linux", Image: Image,
			Binds: binds, Env: map[string]string{"CLDT_MAC": MAC(n.Name), "QEMU_MEMORY": memory, "QEMU_SMP": "4"}}
	}
	return config, nil
}

func (r *Rig) Seed(name string, userdata []byte) error {
	n := r.Topology.MustNode(name)
	if len(n.Interfaces) != 1 {
		return fmt.Errorf("%s needs one NIC", name)
	}
	gateway, ok := lab.Gateway(n.Interfaces[0].Segment)
	if !ok {
		return fmt.Errorf("no gateway for %s", name)
	}
	config, err := scos.Seed(name, n.Interfaces[0].Address, gateway, userdata)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.SeedDir(name), 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.SeedDir(name), "config.ign"), config, 0600)
}

func (r *Rig) Up(ctx context.Context) error {
	if err := scos.ValidateDisk(r.BaseDisk); err != nil {
		return err
	}
	for _, path := range []string{r.BaseDisk, r.AgentISO} {
		if _, err := os.Stat(path); err != nil {
			return err
		}
	}
	for _, n := range r.Topology.Nodes {
		if !n.IsClusterNode() {
			continue
		}
		for _, path := range []string{r.stateDir(n.Name), r.SeedDir(n.Name)} {
			if err := os.MkdirAll(path, 0700); err != nil {
				return err
			}
		}
		if n.Role != lab.ControlPlane {
			if err := r.Seed(n.Name, nil); err != nil {
				return err
			}
		}
	}
	config, err := r.TopologyConfig()
	if err != nil {
		return err
	}
	if r.Runtime != nil {
		if _, err := r.Runtime.Destroy(ctx, r.WorkDir, r.Kind()); err != nil {
			return err
		}
		if err := r.Runtime.Deploy(ctx, r.WorkDir, r.Kind(), r.Topology.Name, config); err != nil {
			return err
		}
		return r.WaitReady(ctx, vm.BootTimeout)
	}
	// Explicit legacy recovery path, used only when no SDK runtime is supplied.
	if err := r.DestroyDeployed(ctx); err != nil {
		return err
	}
	source, err := clab.Source(config)
	if err != nil {
		return err
	}
	if err := os.WriteFile(r.TopologyPath(), source.GetYaml(), 0600); err != nil {
		return err
	}
	_, stderr, code, err := r.Run(ctx, nil, "sudo", "containerlab", "deploy", "-t", r.TopologyPath(), "--reconfigure")
	if err != nil || code != 0 {
		return fmt.Errorf("deploying SCOS: %v, exit %d: %s", err, code, stderr)
	}
	return r.WaitReady(ctx, vm.BootTimeout)
}

// Installed switches subsequent wrapper starts to the installed disk. This is
// recorded only after the official installer has reported installation complete.
func (r *Rig) Installed() error {
	for _, n := range r.Topology.NodesInRole(lab.ControlPlane) {
		if err := os.WriteFile(filepath.Join(r.stateDir(n.Name), "installed"), nil, 0600); err != nil {
			return err
		}
	}
	return nil
}

type node struct {
	rig.Node
	rig *Rig
}

func (n *node) Userdata(ctx context.Context, userdata []byte) error {
	if n.rig.Topology.MustNode(n.Name()).Role != lab.Remote {
		return fmt.Errorf("userdata replacement is only valid for a remote instance")
	}
	if err := n.Kill(ctx); err != nil {
		return err
	}
	if err := n.rig.Seed(n.Name(), userdata); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(n.rig.stateDir(n.Name()), "disk.qcow2")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := n.Boot(ctx); err != nil {
		return err
	}
	return wait.Until(ctx, vm.BootTimeout, n.Name()+" did not boot Ignition", func(ctx context.Context) error {
		_, err := n.Exec(ctx, "true")
		return err
	})
}

func (n *node) Bootstrap(ctx context.Context, data rig.BootstrapData) error {
	if data.Format != rig.Ignition {
		return fmt.Errorf("%s: SCOS VM requires Ignition, got %q", n.Name(), data.Format)
	}
	return n.Userdata(ctx, data.Value)
}
