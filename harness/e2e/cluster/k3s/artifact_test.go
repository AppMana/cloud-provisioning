package k3s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

func TestBuildRequiresPreparedArtifactBeforeTouchingRig(t *testing.T) {
	// An old cache must never substitute for an explicit artifact. The nil
	// Rig also ensures invalid input is rejected before any node is touched.
	d := cluster.Deps{WorkDir: t.TempDir()}
	d.Topology.Nodes = []lab.Node{{Name: "cp", Role: lab.ControlPlane}}
	filename := filepath.Join(d.WorkDir, "k3s-"+Version)
	body := []byte("prepared project build")
	if err := os.WriteFile(filename, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Builder{}).Build(context.Background(), d); err == nil {
		t.Fatal("accepted implicit cached artifact")
	}
	d.K3sBinary = filename
	if err := (Builder{}).Build(context.Background(), d); err == nil {
		t.Fatal("accepted unpinned artifact")
	}
	d.K3sBinarySHA256 = fmt.Sprintf("%x", sha256.Sum256(body))
	got, err := (Builder{}).binary(context.Background(), d)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("prepared build changed: %q %v", got, err)
	}
	if err := os.WriteFile(filename, []byte("different build"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Builder{}).Build(context.Background(), d); err == nil {
		t.Fatal("accepted changed artifact")
	}
}
