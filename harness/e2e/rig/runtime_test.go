package rig

import (
	"context"
	"os"
	"testing"
)

func TestMissingSessionTeardownDoesNotBuildDaemon(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("LABCONTAINERS_LABD", "")
	work := t.TempDir()
	destroyed, err := (LabcontainersRuntime{}).Destroy(context.Background(), work, "vm")
	if err != nil || destroyed {
		t.Fatalf("unexpected missing-session teardown: %v %v", destroyed, err)
	}
	entries, err := os.ReadDir(work)
	if err != nil || len(entries) != 0 {
		t.Fatalf("teardown created build/state files: %v %v", entries, err)
	}
}
