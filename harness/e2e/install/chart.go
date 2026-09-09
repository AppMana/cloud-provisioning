package install

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// stageChart resolves the shipped lock on the network-connected harness host.
// A clean checkout has no ignored charts/ cache, and the bastion cannot fetch
// dependencies. Keep generated archives and Helm caches outside the checkout.
func (p *Product) stageChart(ctx context.Context) (root string, err error) {
	source := filepath.Join(p.RepoDir, "charts", "cloud-provisioning")
	if _, err := os.Stat(filepath.Join(source, "Chart.lock")); err != nil {
		return "", fmt.Errorf("chart requires its pinned Chart.lock: %w", err)
	}
	root, err = os.MkdirTemp(p.WorkDir, "chart-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(root)
		}
	}()
	chart := filepath.Join(root, "cloud-provisioning")
	if err = os.CopyFS(chart, os.DirFS(source)); err != nil {
		return root, fmt.Errorf("staging chart: %w", err)
	}
	lockBytes, err := os.ReadFile(filepath.Join(chart, "Chart.lock"))
	if err != nil {
		return root, err
	}
	var lock struct {
		Dependencies []struct {
			Repository string `json:"repository"`
		} `json:"dependencies"`
	}
	if err = yaml.Unmarshal(lockBytes, &lock); err != nil {
		return root, fmt.Errorf("reading chart lock: %w", err)
	}
	flags := []string{
		"--repository-config", filepath.Join(root, "repositories.yaml"),
		"--repository-cache", filepath.Join(root, "repository-cache")}
	seen := map[string]bool{}
	for _, dependency := range lock.Dependencies {
		repository := dependency.Repository
		if strings.HasPrefix(repository, "oci://") {
			continue
		}
		parsed, parseErr := url.Parse(repository)
		if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
			return root, fmt.Errorf("chart lock requires an explicit HTTP(S) or OCI repository")
		}
		if seen[repository] {
			continue
		}
		args := append([]string{"repo", "add", fmt.Sprintf("cldt-%d", len(seen)), repository}, flags...)
		if out, repoErr := exec.CommandContext(ctx, "helm", args...).CombinedOutput(); repoErr != nil {
			return root, fmt.Errorf("registering locked chart repository: %w: %s", repoErr, out)
		}
		seen[repository] = true
	}
	cmd := exec.CommandContext(ctx, "helm", append([]string{"dependency", "build", chart}, flags...)...)
	if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
		return root, fmt.Errorf("building locked chart dependencies: %w: %s", buildErr, out)
	}
	return root, nil
}
