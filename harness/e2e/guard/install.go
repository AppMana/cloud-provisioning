package guard

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Path is where the guard is installed on the bastion, ahead of the
// distribution's /usr/bin/kubectl on PATH.
const Path = "/usr/local/bin/kubectl"

// Build compiles the guard for the bastion.
func Build(ctx context.Context, moduleDir, workDir string) ([]byte, error) {
	out, err := filepath.Abs(filepath.Join(workDir, "binaries", "kubeguard-linux-amd64"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./cmd/kubeguard")
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if raw, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("building the write guard: %w: %s", err, raw)
	}
	return os.ReadFile(out)
}

// Install puts the guard in front of kubectl on the bastion once the site is
// built, and proves it refuses: a write the policy forbids must fail in the
// guard, before it reaches the API.
func Install(ctx context.Context, bastion rig.Node, binary []byte) error {
	if err := bastion.Put(ctx, bytes.NewReader(binary), Path, 0o755); err != nil {
		return fmt.Errorf("installing the write guard: %w", err)
	}
	out, err := bastion.Exec(ctx, "sh", "-c", "kubectl label node guard-check guard-check= 2>&1; echo exit=$?")
	if err != nil {
		return fmt.Errorf("checking the write guard: %w", err)
	}
	if !strings.Contains(string(out), "harness write guard") || !strings.Contains(string(out), "exit=3") {
		return fmt.Errorf("the write guard is not in front of kubectl on the bastion: %s", out)
	}
	return nil
}
