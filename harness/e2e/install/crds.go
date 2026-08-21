package install

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// CAPIVersion is the Cluster API release whose kinds this lab serves.
const CAPIVersion = "v1.11.1"

// labCRDs are the lab's own infrastructure kinds: the machine and
// cluster a claim names, and the template it is created from.
//
// Embedded rather than read from a path, so the harness carries its
// own API and a run cannot half-work because a file was somewhere
// else.
//
//go:embed crds/containernet.yaml
var labCRDs []byte

// ApplyCRDs installs the kinds the product's reconcilers watch.
//
// Cluster API's controllers are deliberately not installed: the join
// path never talks to them, and the lab's own infrastructure
// controller does what they would have done. But a manager cannot
// start an informer for a kind the API server does not serve, so the
// kinds themselves have to be there.
func (p *Product) ApplyCRDs(ctx context.Context) error {
	if err := p.Kube.Apply(ctx, labCRDs); err != nil {
		return fmt.Errorf("installing the lab's own kinds: %w", err)
	}

	capi, err := p.capiCRDs(ctx)
	if err != nil {
		return err
	}
	if err := p.Kube.Apply(ctx, capi); err != nil {
		return fmt.Errorf("installing Cluster API's kinds: %w", err)
	}
	return nil
}

// capiCRDs fetches Cluster API's release and keeps only its custom
// resource definitions.
//
// Fetched on this host, which has a route out, and applied from the
// bastion, which does not. Filtered to CRDs because the rest of that
// file is the controllers, and running them would mean two things
// reconciling the same machines.
func (p *Product) capiCRDs(ctx context.Context) ([]byte, error) {
	cached := filepath.Join(p.WorkDir, "capi-crds.yaml")
	if body, err := os.ReadFile(cached); err == nil && len(body) > 0 {
		return body, nil
	}

	url := fmt.Sprintf(
		"https://github.com/kubernetes-sigs/cluster-api/releases/download/%s/cluster-api-components.yaml",
		CAPIVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching Cluster API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching Cluster API: %s", resp.Status)
	}
	components, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	crds := OnlyCRDs(components)
	if len(crds) == 0 {
		return nil, fmt.Errorf("the Cluster API release contained no CRDs")
	}
	if err := os.MkdirAll(p.WorkDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(cached, crds, 0o644); err != nil {
		return nil, err
	}
	return crds, nil
}

// OnlyCRDs keeps the CustomResourceDefinition documents of a
// multi-document manifest and drops everything else.
func OnlyCRDs(manifest []byte) []byte {
	var kept []string
	for _, doc := range strings.Split(string(manifest), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		// The kind at the top level of the document, not any string
		// mentioning it: a controller's Deployment names the kinds it
		// watches in its arguments and in RBAC rules.
		if hasTopLevelKind(doc, "CustomResourceDefinition") {
			kept = append(kept, strings.TrimPrefix(doc, "\n"))
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return []byte(strings.Join(kept, "\n---\n") + "\n")
}

func hasTopLevelKind(doc, kind string) bool {
	for _, line := range strings.Split(doc, "\n") {
		if line == "kind: "+kind {
			return true
		}
	}
	return false
}
