package rig

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLabcontainersRuntimeLive(t *testing.T) {
	if os.Getenv("LABCONTAINERS_LIVE") == "" {
		t.Skip("set LABCONTAINERS_LIVE=1 to run the privileged integration test")
	}
	work := t.TempDir()
	topology := filepath.Join(work, "relative.clab.yml")
	if err := os.Mkdir(filepath.Join(work, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(topology, []byte(`name: source
topology:
  defaults:
    kind: linux
    image: alpine:3.20
    network-mode: none
  nodes:
    n1:
      binds: ["data:/data"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runtime := LabcontainersRuntime{}
	if err := runtime.Deploy(ctx, work, "container", "cloud-runtime-smoke", topology); err != nil {
		t.Fatal(err)
	}
	mount, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{(index .Mounts 0).Source}}", "clab-cloud-runtime-smoke-n1").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(mount)), filepath.Join(work, "data"); got != want {
		t.Fatalf("relative bind source = %q, want %q", got, want)
	}
	destroyed, err := runtime.Destroy(ctx, work, "container")
	if err != nil {
		t.Fatal(err)
	}
	if !destroyed {
		t.Fatal("persistent session was not destroyed")
	}
}
