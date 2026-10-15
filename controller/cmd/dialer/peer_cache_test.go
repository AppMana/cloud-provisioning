package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

func TestPeerCacheDoesNotFollowSharedTemporaryPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers-cache.json")
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, path+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := writeCachedPeers(path, tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(other)
	if err != nil || string(data) != "unrelated" {
		t.Fatal("cache write followed a preexisting temporary-path symlink")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("cache permissions are not private")
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".peers-cache.json-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatal("cache write left temporary files")
	}
}
