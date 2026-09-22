package k0s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
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
