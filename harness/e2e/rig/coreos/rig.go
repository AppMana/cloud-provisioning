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

	"sigs.k8s.io/yaml"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/vm"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/vm/scos"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
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

// TopologyYAML reuses the shared appliance/link model and replaces only the
// cluster machines' platform image, boot media and resource requirements.
func (r *Rig) TopologyYAML() ([]byte, error) {
	raw, err := r.Topology.ContainerlabYAML(lab.Container)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	topology := doc["topology"].(map[string]any)
	nodes := topology["nodes"].(map[string]any)
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
		nodes[n.Name] = map[string]any{"kind": "linux", "image": Image,
			"binds": binds, "env": map[string]string{"CLDT_MAC": MAC(n.Name), "QEMU_MEMORY": memory, "QEMU_SMP": "4"}}
	}
	return yaml.Marshal(doc)
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
	raw, err := r.TopologyYAML()
	if err != nil {
		return err
	}
	if err := r.DestroyDeployed(ctx); err != nil {
		return err
	}
	if err := os.WriteFile(r.TopologyPath(), raw, 0600); err != nil {
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
