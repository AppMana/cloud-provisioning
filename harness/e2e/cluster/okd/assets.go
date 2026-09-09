// Package okd uses the release's agent installer and distribution-managed OVN.
package okd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/coreos"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/vm/scos"
)

const InstallerImage = "cloud-provisioning/okd-installer:lab"
const Domain = "okd.cldt.test"

// Topology keeps three schedulable control planes and the two remote slots.
func Topology() lab.Topology {
	t := lab.Default()
	var nodes []lab.Node
	for _, n := range t.Nodes {
		if n.Role != lab.Worker {
			nodes = append(nodes, n)
		}
	}
	t.Nodes = nodes
	return t
}

type Assets struct{ WorkDir, ToolsDir string }

func (a Assets) tool(ctx context.Context, executable string, args ...string) ([]byte, error) {
	work, err := filepath.Abs(a.WorkDir)
	if err != nil {
		return nil, err
	}
	tools, err := filepath.Abs(a.ToolsDir)
	if err != nil {
		return nil, err
	}
	cache := filepath.Join(work, "cache")
	if err := os.MkdirAll(cache, 0700); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("cldt-okd-tool-%d", time.Now().UnixNano())
	defer removeTool(name)
	argv := []string{"run", "--rm", "--name", name, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-e", "HOME=/cache", "-v", work + ":/assets", "-v", tools + ":/tools:ro", "-v", cache + ":/cache",
		"--entrypoint", executable, InstallerImage}
	cmd := exec.CommandContext(ctx, "docker", append(argv, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", executable, err, out)
	}
	return out, nil
}

func removeTool(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
}

// Prepare writes a fresh installation and preserves the official live Ignition
// when adding serial management. No registry credentials are needed for public
// OKD images; the unused auth entry satisfies the installer's schema validation.
func (a Assets) Prepare(ctx context.Context) error {
	if err := os.Mkdir(a.WorkDir, 0700); err != nil {
		return err
	}
	version, err := a.tool(ctx, "/tools/openshift-install", "version")
	if err != nil {
		return err
	}
	if !strings.Contains(string(version), " "+scos.Release+"\n") || !strings.Contains(string(version), "release image "+scos.ReleaseImage+"\n") {
		return fmt.Errorf("installer does not match %s", scos.Release)
	}
	if err := os.WriteFile(filepath.Join(a.WorkDir, "version.txt"), version, 0600); err != nil {
		return err
	}
	write := func(name string, value any) error {
		raw, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(a.WorkDir, name), raw, 0600)
	}
	config := map[string]any{
		"apiVersion": "v1", "baseDomain": "cldt.test", "metadata": map[string]string{"name": "okd"},
		"compute":      []any{map[string]any{"name": "worker", "replicas": 0}},
		"controlPlane": map[string]any{"name": "master", "replicas": 3},
		"networking":   map[string]any{"networkType": "OVNKubernetes", "clusterNetwork": []any{map[string]any{"cidr": "10.244.0.0/16", "hostPrefix": 23}}, "machineNetwork": []any{map[string]string{"cidr": "10.10.0.0/24"}}, "serviceNetwork": []string{"10.96.0.0/16"}},
		"platform":     map[string]any{"baremetal": map[string]any{"apiVIPs": []string{"10.10.0.100"}, "ingressVIPs": []string{"10.10.0.101"}, "provisioningNetwork": "Disabled"}},
		"pullSecret":   `{"auths":{"unused.cldt.invalid":{"auth":"bGFiOmxhYg=="}}}`,
	}
	if err := write("install-config.yaml", config); err != nil {
		return err
	}
	if err := write("install-config.original.json", config); err != nil {
		return err
	}
	var hosts []any
	for _, node := range Topology().NodesInRole(lab.ControlPlane) {
		mac := coreos.MAC(node.Name)
		hosts = append(hosts, map[string]any{"hostname": node.Name, "role": "master",
			"interfaces":      []any{map[string]string{"name": "ens2", "macAddress": mac}},
			"rootDeviceHints": map[string]string{"deviceName": "/dev/vda"},
			"networkConfig": map[string]any{
				"interfaces": []any{map[string]any{"name": "ens2", "type": "ethernet", "state": "up", "mac-address": mac,
					"ipv4": map[string]any{"enabled": true, "dhcp": false, "address": []any{map[string]any{"ip": node.Address(lab.LANSegment), "prefix-length": 24}}}, "ipv6": map[string]bool{"enabled": false}}},
				"dns-resolver": map[string]any{"config": map[string]any{"server": []string{"10.10.0.2"}}},
				"routes":       map[string]any{"config": []any{map[string]any{"destination": "0.0.0.0/0", "next-hop-address": "10.10.0.1", "next-hop-interface": "ens2", "table-id": 254}}},
			}})
	}
	agent := map[string]any{"apiVersion": "v1beta1", "kind": "AgentConfig", "metadata": map[string]string{"name": "okd"}, "rendezvousIP": "10.10.0.10", "hosts": hosts}
	if err := write("agent-config.yaml", agent); err != nil {
		return err
	}
	if err := write("agent-config.original.json", agent); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(a.WorkDir, "openshift"), 0700); err != nil {
		return err
	}
	var management any
	if err := json.Unmarshal(scos.ManagementIgnition, &management); err != nil {
		return err
	}
	for _, role := range []string{"master", "worker"} {
		name := "99-cldt-serial-" + role
		mc := map[string]any{"apiVersion": "machineconfiguration.openshift.io/v1", "kind": "MachineConfig",
			"metadata": map[string]any{"name": name, "labels": map[string]string{"machineconfiguration.openshift.io/role": role}}, "spec": map[string]any{"config": management}}
		if err := write("openshift/"+name+".yaml", mc); err != nil {
			return err
		}
	}
	out, err := a.tool(ctx, "/tools/openshift-install", "agent", "create", "image", "--dir", "/assets", "--log-level", "debug")
	if e := os.WriteFile(filepath.Join(a.WorkDir, "create-image.log"), out, 0600); e != nil {
		return e
	}
	if err != nil {
		return err
	}
	original, err := a.tool(ctx, "coreos-installer", "iso", "ignition", "show", "/assets/agent.x86_64.iso")
	if err != nil {
		return err
	}
	if !json.Valid(original) {
		return fmt.Errorf("installer ISO has no readable Ignition")
	}
	merge := []any{}
	for _, raw := range [][]byte{original, scos.ManagementIgnition} {
		merge = append(merge, map[string]string{"source": "data:;base64," + base64.StdEncoding.EncodeToString(raw)})
	}
	if err := write("combined-live.ign", map[string]any{"ignition": map[string]any{"version": "3.4.0", "config": map[string]any{"merge": merge}}}); err != nil {
		return err
	}
	_, err = a.tool(ctx, "coreos-installer", "iso", "ignition", "embed", "-f", "-i", "/assets/combined-live.ign", "-o", "/assets/agent-serial.iso", "/assets/agent.x86_64.iso")
	return err
}
