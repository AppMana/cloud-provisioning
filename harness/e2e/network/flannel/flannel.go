// Package flannel installs Flannel, routing pod traffic in a vxlan
// tunnel.
//
// Encapsulating, like Cilium, but by a different mechanism and read
// from a different place: the controller's cni package recognises
// Flannel from its own DaemonSet and takes each node's block from
// node.spec.podCIDR, which is the allocation Flannel itself routes
// by. Every builder here allocates node CIDRs, which is Flannel's one
// requirement of the cluster.
package flannel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/network"
)

func init() { network.Register(Installer{}) }

// Manifest is the release this row installs.
const Manifest = "https://github.com/flannel-io/flannel/releases/download/v0.26.3/kube-flannel.yml"

// Installer installs Flannel.
type Installer struct{}

func (Installer) Name() string { return "flannel" }

// Encapsulation is Encapsulated: the vxlan backend addresses its
// packets to nodes, so the tunnel never sees a pod address.
func (Installer) Encapsulation() cni.Encapsulation { return cni.Encapsulated }

// Install carries the plugins and images in, checks the manifest
// agrees with the cluster, applies it, and waits for it to be
// carrying traffic.
func (i Installer) Install(ctx context.Context, d network.Deps) error {
	manifest, err := i.manifest(ctx, d)
	if err != nil {
		return err
	}
	// This network chains the standard bridge plugin, which a node
	// image may not ship and a cloud image never does.
	if err := cluster.EnsureCNIPlugins(ctx, d.Rig, d.WorkDir, network.SiteNodes(d.Topology)); err != nil {
		return err
	}
	if err := i.assertNetwork(manifest, d.PodCIDR); err != nil {
		return err
	}
	if err := i.LoadImages(ctx, d, network.SiteNodes(d.Topology)); err != nil {
		return err
	}
	if err := d.Kube.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("installing Flannel: %w", err)
	}
	if _, err := d.Kube.Run(ctx, "-n", "kube-flannel", "rollout", "status",
		"daemonset/kube-flannel-ds", "--timeout=5m"); err != nil {
		return fmt.Errorf("Flannel never rolled out: %w", err)
	}
	return nil
}

// LoadImages carries Flannel's own images onto the given nodes.
func (i Installer) LoadImages(ctx context.Context, d network.Deps, nodes []string) error {
	manifest, err := i.manifest(ctx, d)
	if err != nil {
		return err
	}
	// The delegated plugin, on a node that has only just acquired a
	// runtime. Install put it on the site's nodes; a remote joined
	// after that and has none, and without it creates no pod sandbox.
	if err := cluster.EnsureCNIPlugins(ctx, d.Rig, d.WorkDir, nodes); err != nil {
		return err
	}
	images := network.ImagesIn(manifest)
	if len(images) == 0 {
		return fmt.Errorf("no images in the Flannel manifest, so nothing would be carried in")
	}
	for _, image := range images {
		if err := d.Images.Load(ctx, image, nodes, nil); err != nil {
			return fmt.Errorf("carrying %s in: %w", image, err)
		}
	}
	return nil
}

// assertNetwork refuses a manifest whose Network is not the
// cluster's.
//
// Flannel routes by the allocation the cluster hands out and by its
// own Network in the same breath; where the two disagree it routes
// nothing and says little. The stock manifest's default happens to be
// this lab's pod CIDR, which is exactly why it is checked rather than
// assumed — a default that changes upstream would change what the row
// proves without changing the row.
func (Installer) assertNetwork(manifest []byte, podCIDR string) error {
	want := fmt.Sprintf("%q: %q", "Network", podCIDR)
	if !strings.Contains(string(manifest), want) {
		return fmt.Errorf("the Flannel manifest's Network is not %s, so it would route by an "+
			"allocation the cluster does not hand out", podCIDR)
	}
	return nil
}

// manifest fetches the pinned release once and caches it.
func (i Installer) manifest(ctx context.Context, d network.Deps) ([]byte, error) {
	path := filepath.Join(d.WorkDir, "kube-flannel.yml")
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		return body, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Manifest, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching Flannel: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching Flannel: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(d.WorkDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return nil, err
	}
	return body, nil
}
