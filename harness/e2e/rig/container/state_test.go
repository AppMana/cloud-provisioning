package container

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// Fresh labs must discard persistent distribution state after removing the
// containers that mounted it. Reused state can retain obsolete cluster members;
// removing a mounted directory before teardown races the running container.
func TestThePreviousLabsStateIsClearedBeforeDeploying(t *testing.T) {
	rec := &recorder{}
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: rec.run}

	if err := r.Up(context.Background()); err != nil {
		t.Fatal(err)
	}

	destroyed, cleared, deployed := -1, -1, -1
	for i, call := range rec.calls {
		joined := strings.Join(call, " ")
		switch {
		case strings.Contains(joined, "containerlab destroy"):
			destroyed = i
		case strings.Contains(joined, "rm -rf") && cleared < 0:
			cleared = i
		case strings.Contains(joined, "containerlab deploy"):
			deployed = i
		}
	}
	if destroyed < 0 || cleared < 0 || deployed < 0 {
		t.Fatalf("destroy=%d clear=%d deploy=%d; all three must happen", destroyed, cleared, deployed)
	}
	if !(destroyed < cleared) {
		t.Error("state was cleared while the containers were still running, which races the mounts")
	}
	if !(cleared < deployed) {
		t.Error("the lab was deployed before the previous run's state was cleared")
	}
}

// Every data root a node declares is cleared, not a list kept in step
// by hand: a distribution added to the topology brings its own.
func TestEveryDataRootIsCleared(t *testing.T) {
	rec := &recorder{}
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: rec.run}
	if err := r.Up(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"var/cp", "var-k0s/cp", "var-rancher/cp"} {
		var found bool
		for _, call := range rec.calls {
			joined := strings.Join(call, " ")
			if strings.Contains(joined, "rm -rf") && strings.Contains(joined, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was never cleared", want)
		}
	}
}
