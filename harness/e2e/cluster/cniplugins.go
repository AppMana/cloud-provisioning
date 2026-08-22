package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// CNIPluginsVersion is the release the lab carries, pinned like every
// other version here.
const CNIPluginsVersion = "v1.6.2"

// CNIPluginsDir is where every network expects to find them.
const CNIPluginsDir = "/opt/cni/bin"

// EnsureCNIPlugins puts the standard plugins on any node without
// them.
//
// Networks that chain the bridge plugin need it there before their
// own agent starts: flannel and kube-router both delegate to it, and
// a node without it creates no pod sandbox at all. The failure is
// every pod on that node stuck in ContainerCreating, which reads as
// the network being broken rather than as a file being absent.
//
// A node image ships some of these and a cloud image ships none, so
// this asks rather than assumes. It is the platform's work, like the
// runtime underneath it.
func EnsureCNIPlugins(ctx context.Context, r rig.Rig, workDir string, nodes []string) error {
	var missing []string
	for _, name := range nodes {
		if _, err := r.Node(name).Exec(ctx, "test", "-x", CNIPluginsDir+"/bridge"); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	archive, err := cniPluginsArchive(ctx, workDir)
	if err != nil {
		return err
	}
	for _, name := range missing {
		node := r.Node(name)
		if err := node.Put(ctx, bytes.NewReader(archive), "/tmp/cni-plugins.tgz", 0o644); err != nil {
			return fmt.Errorf("carrying the CNI plugins onto %s: %w", name, err)
		}
		if _, err := node.Exec(ctx, "sh", "-c",
			"mkdir -p "+CNIPluginsDir+" && tar -C "+CNIPluginsDir+
				" -xzf /tmp/cni-plugins.tgz && rm -f /tmp/cni-plugins.tgz"); err != nil {
			return fmt.Errorf("unpacking the CNI plugins on %s: %w", name, err)
		}
	}
	return nil
}

// cniPluginsArchive fetches the pinned release once and caches it.
func cniPluginsArchive(ctx context.Context, workDir string) ([]byte, error) {
	path := filepath.Join(workDir, "cni-plugins-"+CNIPluginsVersion+".tgz")
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		return body, nil
	}
	url := fmt.Sprintf(
		"https://github.com/containernetworking/plugins/releases/download/%[1]s/cni-plugins-linux-amd64-%[1]s.tgz",
		CNIPluginsVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the CNI plugins: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching the CNI plugins: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return nil, err
	}
	return body, nil
}
