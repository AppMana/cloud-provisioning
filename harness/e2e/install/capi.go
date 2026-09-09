package install

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
)

// Cluster API and the cert-manager it requires, which the product's
// own README names as dependencies before its chart is installed.
//
// They were not installed here for a long time, on the stated grounds
// that "the join path never talks to them". That was wrong, and the
// lab hid it: the product reads Machine.status.nodeRef.name to learn
// which node's pod blocks belong to a remote, and only Cluster API's
// Machine controller ever sets that field. With the controllers
// absent the harness set it instead, so every row proved the join
// against a harness standing in for a dependency rather than against
// the dependency.
const (
	CertManagerVersion = "v1.16.2"
	CertManagerURL     = "https://github.com/cert-manager/cert-manager/releases/download/" +
		CertManagerVersion + "/cert-manager.yaml"
)

// InstallCAPI puts cert-manager and Cluster API on the cluster, in
// that order, because Cluster API's webhooks will not start without
// the certificates cert-manager issues them.
func (p *Product) InstallCAPI(ctx context.Context) error {
	for _, step := range []struct {
		what        string
		url         string
		namespace   string
		deployments []string
	}{
		{"cert-manager", CertManagerURL, "cert-manager",
			[]string{"cert-manager", "cert-manager-webhook", "cert-manager-cainjector"}},
		{"cluster-api", capiURL(), "capi-system",
			[]string{"capi-controller-manager"}},
	} {
		manifest, err := p.fetch(ctx, step.what+".yaml", step.url)
		if err != nil {
			return err
		}
		// Every image, carried in: the site has no route to a registry.
		images := ImagesIn(manifest)
		if len(images) == 0 {
			return fmt.Errorf("%s's manifest names no images", step.what)
		}
		var nodes []string
		for _, n := range cluster.SiteNodes(p.Topology) {
			nodes = append(nodes, n.Name)
		}
		for _, image := range images {
			if err := p.Images.Load(ctx, image, nodes, nil); err != nil {
				return fmt.Errorf("carrying %s in: %w", image, err)
			}
		}
		if err := p.Kube.Apply(ctx, SubstituteDefaults(manifest)); err != nil {
			return fmt.Errorf("installing %s: %w", step.what, err)
		}
		for _, d := range step.deployments {
			if _, err := p.Kube.Run(ctx, "-n", step.namespace, "rollout", "status",
				"deployment/"+d, "--timeout=6m"); err != nil {
				return fmt.Errorf("%s's %s did not become available: %w", step.what, d, err)
			}
		}
	}
	return nil
}

func capiURL() string {
	return fmt.Sprintf(
		"https://github.com/kubernetes-sigs/cluster-api/releases/download/%s/cluster-api-components.yaml",
		CAPIVersion)
}

// fetch downloads once and caches, because this host has a route out
// and the site does not.
func (p *Product) fetch(ctx context.Context, name, url string) ([]byte, error) {
	cached := filepath.Join(p.WorkDir, name)
	if body, err := os.ReadFile(cached); err == nil && len(body) > 0 {
		return body, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: %s", name, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(p.WorkDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(cached, body, 0o644); err != nil {
		return nil, err
	}
	return body, nil
}

// Both forms occur in a real manifest: image as a later key of a list
// item, and image as its first, where YAML puts the dash on the same
// line.
var imageLine = regexp.MustCompile(`(?m)^\s*-?\s*image:\s*(\S+)\s*$`)

// ImagesIn lists every image a manifest names, deduplicated.
func ImagesIn(manifest []byte) []string {
	seen := map[string]bool{}
	for _, m := range imageLine.FindAllStringSubmatch(string(manifest), -1) {
		image := strings.Trim(m[1], `"'`)
		// A manifest may template an image it does not name.
		if image == "" || strings.Contains(image, "{{") {
			continue
		}
		seen[image] = true
	}
	out := make([]string, 0, len(seen))
	for image := range seen {
		out = append(out, image)
	}
	sort.Strings(out)
	return out
}

// clusterctlVar matches the variables Cluster API's release file
// carries, in the form ${NAME} or ${NAME:=default}.
var clusterctlVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::=([^}]*))?\}`)

// SubstituteDefaults resolves the variables Cluster API's release
// leaves in its manifest.
//
// The published components.yaml is not directly appliable: it carries
// ${CAPI_INSECURE_DIAGNOSTICS:=false} and its like, which clusterctl
// substitutes at install time. Applied raw, the controller starts
// with the literal text as a flag value and crash-loops on
// ParseBool — which is what happened here, and reads as Cluster API
// being broken rather than as the manifest being a template.
//
// This is clusterctl's substitution and not a lab convenience: an
// operator following the README runs clusterctl, which does exactly
// this. A variable with no default is left as it is, so that
// something genuinely unset fails loudly rather than becoming an
// empty string that means something else.
func SubstituteDefaults(manifest []byte) []byte {
	return clusterctlVar.ReplaceAllFunc(manifest, func(m []byte) []byte {
		groups := clusterctlVar.FindSubmatch(m)
		if value, ok := os.LookupEnv(string(groups[1])); ok {
			return []byte(value)
		}
		if groups[2] != nil {
			return groups[2]
		}
		return m
	})
}
