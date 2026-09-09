package install

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageChartBuildsLockedDependencyWithoutSourceCache(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("native Helm required")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repository")
	source := filepath.Join(root, "source")
	chart := filepath.Join(source, "charts", "cloud-provisioning")
	child := filepath.Join(root, "child")
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(child, "Chart.yaml"), "apiVersion: v2\nname: child\nversion: 1.2.3\n")
	write(filepath.Join(child, "templates", "marker.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: locked-dependency-marker\n")
	helm := func(args ...string) []byte {
		t.Helper()
		out, err := exec.Command("helm", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("Helm failed: %v: %s", err, out)
		}
		return out
	}
	helm("package", child, "--destination", repo)
	server := httptest.NewServer(http.FileServer(http.Dir(repo)))
	defer server.Close()
	helm("repo", "index", repo, "--url", server.URL)
	write(filepath.Join(chart, "Chart.yaml"), fmt.Sprintf("apiVersion: v2\nname: cloud-provisioning\nversion: 0.1.0\ndependencies:\n- name: child\n  version: 1.2.3\n  repository: %s\n", server.URL))
	helm("dependency", "update", chart, "--repository-config", filepath.Join(root, "initial-repositories.yaml"), "--repository-cache", filepath.Join(root, "initial-cache"))
	lock, err := os.ReadFile(filepath.Join(chart, "Chart.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(chart, "charts")); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	p := &Product{RepoDir: source, WorkDir: work}
	stage, err := p.stageChart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	rendered := helm("template", "test", filepath.Join(stage, "cloud-provisioning"))
	if !strings.Contains(string(rendered), "name: locked-dependency-marker") {
		t.Fatal("locked dependency missing from rendered chart")
	}
	if _, err := os.Stat(filepath.Join(chart, "charts")); !os.IsNotExist(err) {
		t.Fatal("staging modified source dependency cache")
	}
	after, err := os.ReadFile(filepath.Join(chart, "Chart.lock"))
	if err != nil || string(after) != string(lock) {
		t.Fatal("staging changed source lock")
	}
	// A changed dependency declaration must fail against the existing lock,
	// rather than silently generating a replacement lock or using a stale tgz.
	body, err := os.ReadFile(filepath.Join(chart, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(chart, "Chart.yaml"), strings.Replace(string(body), "version: 1.2.3", "version: 2.0.0", 1))
	if _, err := p.stageChart(context.Background()); err == nil {
		t.Fatal("accepted stale lock")
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatal("failed staging left artifacts")
	}
}

func TestStageChartRequiresLockBeforeCreatingArtifacts(t *testing.T) {
	work := t.TempDir()
	p := &Product{RepoDir: t.TempDir(), WorkDir: work}
	if _, err := p.stageChart(context.Background()); err == nil {
		t.Fatal("accepted missing lock")
	}
	entries, err := os.ReadDir(work)
	if err != nil || len(entries) != 0 {
		t.Fatal("created artifacts without lock")
	}
}
