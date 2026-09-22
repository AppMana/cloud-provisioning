package rig

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srl-labs/containerlab/core"
	"github.com/srl-labs/containerlab/types"
)

func TestLabcontainersRuntimeLive(t *testing.T) {
	if os.Getenv("LABCONTAINERS_LIVE") == "" {
		t.Skip("set LABCONTAINERS_LIVE=1 to run the privileged integration test")
	}
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	topology := &core.Config{Name: "source", Topology: &types.Topology{
		Defaults: &types.NodeDefinition{Kind: "linux", Image: "alpine:3.20"},
		Nodes:    map[string]*types.NodeDefinition{"n1": {Binds: []string{"data:/data"}}},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runtime := LabcontainersRuntime{}
	name := fmt.Sprintf("cloud-runtime-%d", time.Now().UnixNano())
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if _, err := runtime.Destroy(cleanup, work, "container"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	if err := runtime.Deploy(ctx, work, "container", name, topology); err != nil {
		t.Fatal(err)
	}
	mount, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{(index .Mounts 0).Source}}", "clab-"+name+"-n1").Output()
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
