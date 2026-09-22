package container

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/srl-labs/containerlab/core"
)

type missingSessionRuntime struct {
	deployErr error
	deploys   int
}

func (r *missingSessionRuntime) Destroy(context.Context, string, string) (bool, error) {
	return false, nil
}
func (r *missingSessionRuntime) Deploy(context.Context, string, string, string, *core.Config) error {
	r.deploys++
	return r.deployErr
}

func TestMissingSessionNeverFallsBackToNativeDestroy(t *testing.T) {
	runtime := &missingSessionRuntime{deployErr: errors.New("deployment boundary")}
	r := New(lab.Default(), t.TempDir())
	r.Runtime = runtime
	r.Run = func(_ context.Context, _ io.Reader, argv ...string) ([]byte, []byte, int, error) {
		t.Fatalf("unexpected host command without session ownership: %v", argv)
		return nil, nil, 0, nil
	}
	if err := r.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Up(context.Background()); !errors.Is(err, runtime.deployErr) {
		t.Fatalf("expected owned deployment boundary, got %v", err)
	}
	if runtime.deploys != 1 {
		t.Fatalf("deploys = %d", runtime.deploys)
	}
}

func TestMissingSessionPreservesExistingBindData(t *testing.T) {
	runtime := &missingSessionRuntime{}
	r := New(lab.Default(), t.TempDir())
	r.Runtime = runtime
	r.Run = func(_ context.Context, _ io.Reader, argv ...string) ([]byte, []byte, int, error) {
		t.Fatalf("unexpected host mutation: %v", argv)
		return nil, nil, 0, nil
	}
	marker := filepath.Join(r.WorkDir, "var", "cp", "preserve")
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("unowned state"), 0600); err != nil {
		t.Fatal(err)
	}
	err := r.Up(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unowned bind data") {
		t.Fatalf("unexpected result: %v", err)
	}
	if runtime.deploys != 0 {
		t.Fatal("deployed with unowned state")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "unowned state" {
		t.Fatalf("unowned state changed: %q %v", data, err)
	}
}
