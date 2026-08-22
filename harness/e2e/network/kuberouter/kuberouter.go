// Package kuberouter installs kube-router, routing pod addresses
// natively over BGP.
//
// The row worth having in the matrix, and the reason is an
// interaction rather than a feature: this operator's dialer refuses
// BGP across the tunnel by design, because the tunnel does not carry
// the network's control plane. So kube-router's full mesh forms among
// the site's nodes only, and a remote's pod blocks reach the site
// through the operator's own mechanism — accept lists on the
// endpoints, derived transit routes on everyone else. If kube-router
// fights those routes rather than coexisting with them, this is the
// row where it shows.
//
// This is the CNI-and-routing variant, which keeps kube-proxy: every
// builder here runs one, and a row that replaced it would be testing
// a different cluster.
package kuberouter

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
const Manifest = "https://raw.githubusercontent.com/cloudnativelabs/kube-router/v2.4.0/daemonset/kubeadm-kuberouter.yaml"

// Installer installs kube-router.
type Installer struct{}

func (Installer) Name() string { return "kube-router" }

// Encapsulation is Native: it announces each node's block over BGP
// and routes pod addresses between nodes unchanged.
func (Installer) Encapsulation() cni.Encapsulation { return cni.Native }

// Install carries the plugins and images in, sets the one thing the
// stock manifest leaves wrong for this lab, applies it, and waits.
func (i Installer) Install(ctx context.Context, d network.Deps) error {
	manifest, err := i.manifest(ctx, d)
	if err != nil {
		return err
	}
	if err := cluster.EnsureCNIPlugins(ctx, d.Rig, d.WorkDir, network.SiteNodes(d.Topology)); err != nil {
		return err
	}
	hairpinned, err := hairpin(manifest)
	if err != nil {
		return err
	}
	if err := i.LoadImages(ctx, d, network.SiteNodes(d.Topology)); err != nil {
		return err
	}
	if err := d.Kube.Apply(ctx, hairpinned); err != nil {
		return fmt.Errorf("installing kube-router: %w", err)
	}
	if _, err := d.Kube.Run(ctx, "-n", "kube-system", "rollout", "status",
		"daemonset/kube-router", "--timeout=5m"); err != nil {
		return fmt.Errorf("kube-router never rolled out: %w", err)
	}
	return nil
}

// LoadImages carries kube-router's own images onto the given nodes.
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
		return fmt.Errorf("no images in the kube-router manifest, so nothing would be carried in")
	}
	for _, image := range images {
		if err := d.Images.Load(ctx, image, nodes, nil); err != nil {
			return fmt.Errorf("carrying %s in: %w", image, err)
		}
	}
	return nil
}

// hairpin makes the bridge reflect a frame back out the port it
// arrived on.
//
// A pod that dials its own service is DNATed straight back to itself,
// and without hairpin on its bridge port that frame has nowhere to
// go. The stock conf leaves the bridge plugin's hairpinMode at its
// false default; Flannel's conf sets it true and Calico has no bridge
// to tell, so this is the only network here that needs saying.
// kube-proxy already owns the NAT half — this variant runs with
// --run-service-proxy=false and kube-proxy masquerades the hairpin
// flow — so the bridge port is the only half missing.
//
// A manifest that has changed shape is a failure rather than a
// silent no-op: unset, the row fails later as a handful of service
// checks, which reads as the network being broken rather than as one
// field being absent.
func hairpin(manifest []byte) ([]byte, error) {
	const anchor = `"isDefaultGateway":true,`
	out := strings.Replace(string(manifest), anchor,
		anchor+"\n             \"hairpinMode\":true,", 1)
	if !strings.Contains(out, `"hairpinMode":true`) {
		return nil, fmt.Errorf("the kube-router manifest's bridge conf has changed shape, " +
			"so hairpinMode was not set and a pod dialling its own service would have nowhere to go")
	}
	return []byte(out), nil
}

// manifest fetches the pinned release once and caches it.
func (i Installer) manifest(ctx context.Context, d network.Deps) ([]byte, error) {
	path := filepath.Join(d.WorkDir, "kube-router.yaml")
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		return body, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Manifest, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching kube-router: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching kube-router: %s", resp.Status)
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
