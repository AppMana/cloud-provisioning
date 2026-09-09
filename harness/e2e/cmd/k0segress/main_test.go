package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRenderPreservesExistingEvidence(t *testing.T) {
	out := filepath.Join(t.TempDir(), "addon.json")
	fixture := "../../cluster/k0s/testdata/konnectivity-native.json"
	if err := render(fixture, "cp2,cp3", out); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := render(fixture, "cp,cp2", out); err == nil {
		t.Fatal("overwrote earlier manifest")
	}
	after, _ := os.ReadFile(out)
	if !bytes.Equal(before, after) {
		t.Fatal("existing manifest changed")
	}
}
