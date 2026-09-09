package k0s

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

// InstallDefaultRouteAgents is an explicit mixed-OS site addon. It runs after
// the distro network is Ready, before installing CAPI or claiming remote nodes.
// It is intentionally separate from Build so the unmodified distro baseline
// remains available to the matrix.
func InstallDefaultRouteAgents(ctx context.Context, d cluster.Deps) error {
	cps := d.Topology.NodesInRole(lab.ControlPlane)
	if len(cps) < 2 {
		return fmt.Errorf("Linux default-route addon requires at least two controllers")
	}
	hosts := make([]string, 0, len(cps))
	for _, cp := range cps {
		osName, err := d.Kube.Get(ctx, "", "node", cp.Name, "{.metadata.labels.kubernetes\\.io/os}")
		if err != nil || osName != "linux" {
			return fmt.Errorf("default-route node %s must be registered Linux: %v", cp.Name, err)
		}
		hostname, err := d.Kube.Get(ctx, "", "node", cp.Name, "{.metadata.labels.kubernetes\\.io/hostname}")
		if err != nil {
			return err
		}
		hosts = append(hosts, hostname)
		ports, err := d.Rig.Node(cp.Name).Exec(ctx, "ss", "-H", "-ltn", "( sport = :18095 or sport = :18096 )")
		if err != nil || strings.TrimSpace(string(ports)) != "" {
			return fmt.Errorf("default-route ports on %s unavailable: %v", cp.Name, err)
		}
	}
	native, err := d.Kube.Run(ctx, "-n", "kube-system", "get", "daemonset", "konnectivity-agent", "-o", "json")
	if err != nil {
		return err
	}
	manifest, err := DefaultRouteAgents(native, hosts)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(d.WorkDir, "k0s-default-route-native.json"), native, 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(d.WorkDir, "k0s-default-route-addon.json"), manifest, 0644); err != nil {
		return err
	}
	if err := d.Kube.Apply(ctx, manifest); err != nil {
		return err
	}
	if _, err := d.Kube.Run(ctx, "-n", "kube-system", "rollout", "status", "daemonset/"+DefaultRouteAgentName, "--timeout=120s"); err != nil {
		return err
	}
	// Ready alone does not establish a connection to every controller.
	return wait.Until(ctx, 2*time.Minute, "Linux default-route server connections", func(ctx context.Context) error {
		want := fmt.Sprintf("konnectivity_network_proxy_agent_open_server_connections %d", len(cps))
		for _, cp := range cps {
			raw, err := d.Rig.Node(cp.Name).Exec(ctx, "curl", "--fail", "--silent", "--show-error", "--max-time", "5", "http://127.0.0.1:18095/metrics")
			if err != nil {
				return err
			}
			found := false
			for _, line := range strings.Split(string(raw), "\n") {
				if line == want {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("default-route agent on %s has not connected to all %d servers", cp.Name, len(cps))
			}
		}
		return nil
	})
}
