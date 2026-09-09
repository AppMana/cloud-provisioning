// Package cilium installs Cilium, routing pod traffic in a vxlan
// tunnel.
//
// The encapsulating half of the matrix, and the reason the matrix has
// two halves at all. Calico here routes pod addresses natively, so
// the mesh's accept lists carry each node's pod blocks; Cilium
// addresses its packets to nodes, so they carry node addresses and no
// blocks at all. The controller's cni package decides which of those
// a cluster is by reading the network's own resources, and a row that
// installed only one of them would never test that it can tell.
//
// Rendered here rather than installed from a chart repository,
// because the bastion has no route to one: this host does, so the
// document is produced here and applied like any other manifest.
package cilium

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/harness/e2e/network"
)

func init() { network.Register(Installer{}) }

// Version is the release this row installs. Pinned: a network that
// changes under the matrix makes two runs incomparable.
const Version = "1.16.5"

// Installer installs Cilium.
type Installer struct{}

func (Installer) Name() string { return "cilium" }

// Encapsulation is Encapsulated: this row routes in a vxlan tunnel,
// so the mesh carries node addresses and no pod blocks.
func (Installer) Encapsulation() cni.Encapsulation { return cni.Encapsulated }

// Install renders the chart, carries its images in, applies it, and
// waits for it to be carrying traffic.
func (i Installer) Install(ctx context.Context, d network.Deps) error {
	manifest, err := i.manifest(ctx, d)
	if err != nil {
		return err
	}
	if err := i.LoadImages(ctx, d, network.SiteNodes(d.Topology)); err != nil {
		return err
	}
	if err := d.Kube.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("installing Cilium: %w", err)
	}
	// Rolled out, not merely applied: an agent that has not started
	// has programmed no datapath, and the first thing to notice would
	// be every pod check failing at once.
	if _, err := d.Kube.Run(ctx, "-n", "kube-system", "rollout", "status",
		"daemonset/cilium", "--timeout=8m"); err != nil {
		return fmt.Errorf("Cilium never rolled out: %w", err)
	}
	return nil
}

// LoadImages carries Cilium's own images onto the given nodes.
func (i Installer) LoadImages(ctx context.Context, d network.Deps, nodes []string) error {
	manifest, err := i.manifest(ctx, d)
	if err != nil {
		return err
	}
	images := network.ImagesIn(manifest)
	if len(images) == 0 {
		return fmt.Errorf("no images in the Cilium render, so nothing would be carried in")
	}
	for _, image := range images {
		if err := d.Images.Load(ctx, image, nodes, nil); err != nil {
			return fmt.Errorf("carrying %s in: %w", image, err)
		}
	}
	return nil
}

// manifest renders the chart once and caches it.
//
// The values are the row's claims about this network, and each one is
// load-bearing:
//
// cluster-pool IPAM pinned to the lab's own pod CIDR, so the
// allocator and the model agree — the same reason Calico's pool is
// pinned, and the same failure if it is not.
//
// tunnel/vxlan, which is Cilium's default and what makes this row the
// encapsulating one. Set explicitly rather than relied on, because a
// default that changes under the matrix changes what the row proves
// without changing the row.
//
// kube-proxy stays. Replacing it is Cilium's business on clusters
// built for that, and every builder here runs kube-proxy; a row that
// removed it would be testing a different cluster.
func (i Installer) manifest(ctx context.Context, d network.Deps) ([]byte, error) {
	path := filepath.Join(d.WorkDir, "cilium-"+Version+".yaml")
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		return body, nil
	}

	cmd := exec.CommandContext(ctx, "helm", "template", "cilium", "cilium",
		"--repo", "https://helm.cilium.io",
		"--version", Version,
		"--namespace", "kube-system",
		"--set", "operator.replicas=1",
		"--set", "ipam.mode=cluster-pool",
		"--set", "ipam.operator.clusterPoolIPv4PodCIDRList={"+d.PodCIDR+"}",
		"--set", "routingMode=tunnel",
		"--set", "tunnelProtocol=vxlan",
	)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("rendering Cilium %s: %w: %s", Version, err, errb.String())
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("rendering Cilium %s produced nothing", Version)
	}
	if err := os.MkdirAll(d.WorkDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
