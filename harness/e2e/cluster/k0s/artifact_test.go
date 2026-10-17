package k0s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

func TestBinaryRequiresPreparedContentPin(t *testing.T) {
	body := []byte("prepared fork build, not an upstream fallback")
	filename := filepath.Join(t.TempDir(), "k0s")
	if err := os.WriteFile(filename, body, 0o600); err != nil {
		t.Fatal(err)
	}
	pin := fmt.Sprintf("%x", sha256.Sum256(body))
	for _, d := range []cluster.Deps{
		{},
		{K0sBinary: filename},
		{K0sBinarySHA256: pin},
		{K0sBinary: filename, K0sBinarySHA256: "invalid"},
		{K0sBinary: filename, K0sBinarySHA256: strings.Repeat("0", 64)},
		{K0sBinary: filename + "-absent", K0sBinarySHA256: pin},
	} {
		if _, err := (Builder{}).binary(context.Background(), d); err == nil {
			t.Fatal("accepted missing or unpinned artifact", d.K0sBinary)
		}
	}
	d := cluster.Deps{K0sBinary: filename, K0sBinarySHA256: pin}
	got, err := (Builder{}).binary(context.Background(), d)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("did not consume the prepared artifact unchanged: %q %v", got, err)
	}
	if err := os.WriteFile(filename, []byte("different build"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Builder{}).binary(context.Background(), d); err == nil {
		t.Fatal("accepted changed artifact")
	}
}

func TestKubeletArgumentsAreOneNativeArgument(t *testing.T) {
	got := siteKubeletArgs("calico-site-bgp", "192.0.2.1")
	want := "--kubelet-extra-args=--node-ip=192.0.2.1 --node-labels=" + siteCalicoLabel + "=" + siteCalicoValue
	if got != want {
		t.Fatalf("native argument = %q, want %q", got, want)
	}
}

// A content pin alone does not prove that bytes identify the selected release.
// Simulated staging proves pre-start rejection, not guest execution or health.
func TestBuildRejectsWrongStagedVersionBeforeStartingRuntime(t *testing.T) {
	body := []byte("prepared bytes")
	filename := filepath.Join(t.TempDir(), "k0s")
	if err := os.WriteFile(filename, body, 0600); err != nil {
		t.Fatal(err)
	}
	n := &stagedVersionNode{t: t}
	d := cluster.Deps{Topology: lab.Default(), Rig: stagedVersionRig{node: n},
		K0sBinary: filename, K0sBinarySHA256: fmt.Sprintf("%x", sha256.Sum256(body))}
	if err := (Builder{}).Build(context.Background(), d); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected pre-start version rejection, got %v", err)
	}
	if n.staged == 0 {
		t.Fatal("did not exercise staging")
	}
}

type stagedVersionNode struct {
	rig.Node
	t      *testing.T
	staged int
}

func (n *stagedVersionNode) Put(_ context.Context, _ io.Reader, path string, _ fs.FileMode) error {
	if path != "/usr/local/bin/k0s" {
		n.t.Fatalf("configured runtime before version admission: %s", path)
	}
	n.staged++
	return nil
}
func (n *stagedVersionNode) Exec(_ context.Context, args ...string) ([]byte, error) {
	if strings.Join(args, " ") != "k0s version" {
		n.t.Fatalf("unexpected runtime command: %v", args)
	}
	return []byte("v1.35.0+k0s.1"), nil
}

type stagedVersionRig struct {
	rig.Rig
	node *stagedVersionNode
}

func (r stagedVersionRig) Node(string) rig.Node { return r.node }
