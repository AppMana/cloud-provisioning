package rke2

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

func TestAllArtifactsValidatedBeforeTouchingNodes(t *testing.T) {
	d := cluster.Deps{RKE2ArtifactsDirectory: t.TempDir()}
	d.Topology.Nodes = []lab.Node{{Name: "cp", Role: lab.ControlPlane}}
	bodies := map[string][]byte{
		"install.sh":              []byte("#!/bin/sh\nexit 0\n"),
		"rke2.linux-amd64.tar.gz": {0, 255, 1, 2},
		"sha256sum-amd64.txt":     []byte("prepared checksum file"),
	}
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(d.RKE2ArtifactsDirectory, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A nil Rig catches attempts to mutate a node before complete validation.
	if err := (Builder{}).Build(context.Background(), d); err == nil {
		t.Fatal("accepted unpinned files")
	}
	d.RKE2InstallerSHA256 = fmt.Sprintf("%x", sha256.Sum256(bodies["install.sh"]))
	d.RKE2ArchiveSHA256 = fmt.Sprintf("%x", sha256.Sum256(bodies["rke2.linux-amd64.tar.gz"]))
	if err := (Builder{}).Build(context.Background(), d); err == nil {
		t.Fatal("accepted partial pins")
	}
	d.RKE2ChecksumsSHA256 = fmt.Sprintf("%x", sha256.Sum256(bodies["sha256sum-amd64.txt"]))
	got, err := preparedArtifacts(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range bodies {
		if !bytes.Equal(got[name], body) {
			t.Fatal("changed artifact", name)
		}
	}
	for name, body := range bodies {
		filename := filepath.Join(d.RKE2ArtifactsDirectory, name)
		if err := os.WriteFile(filename, []byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := (Builder{}).Build(context.Background(), d); err == nil {
			t.Fatal("accepted changed artifact", name)
		}
		if err := os.WriteFile(filename, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
