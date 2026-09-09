package cluster

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// CRICTLVersion is the release the lab carries. Pinned, like every
// other version here: a tool that changes under the matrix makes two
// runs incomparable.
const CRICTLVersion = "v1.34.0"

// CRICTLPath is where a node keeps it.
const CRICTLPath = "/usr/local/bin/crictl"

// EnsureCRICTL puts crictl on any node that has none.
//
// The reachability matrix runs its probes inside the pod that is
// being measured from, and reaches that pod through the node's own
// CRI rather than through the API server — deliberately, because an
// outage row is often measuring the moment the API path is broken,
// and a check that went through it would be measuring itself.
//
// A Kubernetes node image ships the tool for that. A cloud image does
// not, and neither does k0s, whose bundled bin/ holds a kubelet, runc
// and iptables and nothing else. So on machines all 140 checks failed
// identically with "crictl: command not found", which reads like a
// lab with no connectivity at all rather than a lab with no tool.
//
// Carrying it is the platform's work — the same as carrying the
// distribution's own binary — and not the product's. Nodes that
// already have one are left alone, which is every node on the
// container rig.
func EnsureCRICTL(ctx context.Context, r rig.Nodes, workDir string, nodes []string) error {
	var missing []string
	for _, name := range nodes {
		if _, err := r.Node(name).Exec(ctx, "sh", "-c", "command -v crictl"); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	binary, err := crictlBinary(ctx, workDir)
	if err != nil {
		return err
	}
	for _, name := range missing {
		if err := r.Node(name).Put(ctx, bytes.NewReader(binary), CRICTLPath, 0o755); err != nil {
			return fmt.Errorf("carrying crictl onto %s: %w", name, err)
		}
	}
	return nil
}

// crictlBinary fetches the pinned release once and caches it, because
// this host has a route out and the lab's nodes reach only what their
// own edges explain.
func crictlBinary(ctx context.Context, workDir string) ([]byte, error) {
	path := filepath.Join(workDir, "crictl-"+CRICTLVersion)
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		return body, nil
	}

	url := fmt.Sprintf(
		"https://github.com/kubernetes-sigs/cri-tools/releases/download/%[1]s/crictl-%[1]s-linux-amd64.tar.gz",
		CRICTLVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching crictl: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching crictl: %s", resp.Status)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading the crictl archive: %w", err)
	}
	defer gz.Close()

	archive := tar.NewReader(gz)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the crictl archive: %w", err)
		}
		if filepath.Base(header.Name) != "crictl" {
			continue
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			return nil, fmt.Errorf("reading crictl: %w", err)
		}
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return nil, err
		}
		return body, nil
	}
	return nil, fmt.Errorf("the crictl archive carries no crictl")
}
