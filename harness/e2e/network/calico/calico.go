// Package calico installs Calico, routing pod addresses natively.
//
// Native is the point: this mesh carries pod traffic unencapsulated,
// so the tunnel sees packets addressed to pods and each peer's accept
// list carries the blocks its node owns. The stock manifest
// encapsulates, so the pool is changed and the daemonset restarted —
// and the restart is not optional, for the reason below.
package calico

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/network"
)

func init() { network.Register(Installer{}) }

// Manifest is the release this row installs. Pinned: a network that
// changes under the matrix makes two runs incomparable.
const Manifest = "https://raw.githubusercontent.com/projectcalico/calico/v3.29.1/manifests/calico.yaml"

// Installer installs Calico.
type Installer struct{}

func (Installer) Name() string { return "calico" }

// Install fetches the manifest on this host, carries its images in,
// applies it, and then makes it native.
func (i Installer) Install(ctx context.Context, d network.Deps) error {
	manifest, err := i.manifest(ctx, d)
	if err != nil {
		return err
	}

	// Onto the site's nodes only. A remote has no runtime of its own
	// until it joins, which is after this; its images arrive then,
	// through LoadImages.
	if err := i.LoadImages(ctx, d, network.SiteNodes(d.Topology)); err != nil {
		return err
	}

	if err := d.Kube.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("installing Calico: %w", err)
	}

	// Autodetect the address a node reaches the cluster by, not the
	// first interface that has one.
	//
	// On a site node the two agree. On a remote they cannot:
	// first-found lands on the interface facing the internet, an
	// address the site has no route to, and Calico's address monitor
	// re-detects at every interface change — so it would restate that
	// address exactly when a tunnel moves. can-reach follows the route
	// to the API server, which on a remote is the tunnel, so the
	// monitor's own re-detection converges on the address the mesh
	// gave the node.
	if _, err := d.Kube.Run(ctx, "-n", "kube-system", "set", "env", "daemonset/calico-node",
		"IP_AUTODETECTION_METHOD=can-reach="+d.APIServer); err != nil {
		return fmt.Errorf("setting Calico's autodetection method: %w", err)
	}

	if err := i.makeNative(ctx, d); err != nil {
		return err
	}
	return nil
}

// LoadImages carries Calico's own images onto the given nodes.
func (i Installer) LoadImages(ctx context.Context, d network.Deps, nodes []string) error {
	manifest, err := i.manifest(ctx, d)
	if err != nil {
		return err
	}
	images := imagesIn(manifest)
	if len(images) == 0 {
		return fmt.Errorf("no images in the Calico manifest, so nothing would be carried in")
	}
	for _, image := range images {
		if err := d.Images.Load(ctx, image, nodes, nil); err != nil {
			return fmt.Errorf("carrying %s in: %w", image, err)
		}
	}
	return nil
}

// makeNative turns encapsulation off and restarts the daemonset.
func (i Installer) makeNative(ctx context.Context, d network.Deps) error {
	const pool = "ippools.crd.projectcalico.org"
	deadline := time.Now().Add(4 * time.Minute)
	for {
		if _, err := d.Kube.Run(ctx, "get", pool, "default-ipv4-ippool"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Calico never created its default pool")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}

	if _, err := d.Kube.Run(ctx, "patch", pool, "default-ipv4-ippool", "--type", "merge",
		"-p", `{"spec":{"ipipMode":"Never","vxlanMode":"Never"}}`); err != nil {
		return fmt.Errorf("setting the pool's mode: %w", err)
	}

	// Waited for, not fired and forgotten. calico-node programs its
	// routes from the pool it saw when it started, so a pool changed
	// afterwards leaves the old encapsulation's routes in place. Node
	// readiness is already satisfied by the pods running with the old
	// routes, so without this the lab reports a cluster whose routes
	// and whose model disagree — and says nothing.
	if _, err := d.Kube.Run(ctx, "-n", "kube-system", "rollout", "restart", "daemonset/calico-node"); err != nil {
		return fmt.Errorf("restarting calico-node: %w", err)
	}
	if _, err := d.Kube.Run(ctx, "-n", "kube-system", "rollout", "status",
		"daemonset/calico-node", "--timeout=5m"); err != nil {
		return fmt.Errorf("calico-node did not come back after the pool changed: %w", err)
	}
	return nil
}

// manifest fetches the release once and caches it, because this host
// has a route out and the site does not.
func (i Installer) manifest(ctx context.Context, d network.Deps) ([]byte, error) {
	path := filepath.Join(d.WorkDir, "calico.yaml")
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		return body, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Manifest, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching Calico: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching Calico: %s", resp.Status)
	}
	var body []byte
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	if err := os.MkdirAll(d.WorkDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return nil, err
	}
	return body, nil
}

// Both forms occur in a real manifest: image as a later key of a
// list item, and image as its first, where YAML puts the dash on the
// same line.
var imageLine = regexp.MustCompile(`(?m)^\s*-?\s*image:\s*(\S+)\s*$`)

// imagesIn lists every image a manifest names, deduplicated.
func imagesIn(manifest []byte) []string {
	seen := map[string]bool{}
	for _, m := range imageLine.FindAllStringSubmatch(string(manifest), -1) {
		seen[strings.Trim(m[1], `"'`)] = true
	}
	out := make([]string, 0, len(seen))
	for image := range seen {
		out = append(out, image)
	}
	sort.Strings(out)
	return out
}
