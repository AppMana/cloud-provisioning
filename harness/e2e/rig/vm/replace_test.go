package vm

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// The first compact OKD deployment left the prior site's w1/w2 containers
// behind because --reconfigure destroyed only nodes in the new topology.
func TestReplacementDestroysOldTopologyBeforeOverwritingIt(t *testing.T) {
	r := New(lab.Default(), t.TempDir())
	old := []byte("old deployed topology includes w1 and w2\n")
	if err := os.WriteFile(r.TopologyPath(), old, 0600); err != nil {
		t.Fatal(err)
	}
	destroyed := false
	r.Run = func(_ context.Context, _ io.Reader, args ...string) ([]byte, []byte, int, error) {
		command := strings.Join(args, " ")
		switch command {
		case "docker ps -aq --filter label=containerlab=cldt":
			return []byte("old-worker-id\n"), nil, 0, nil
		case "sudo containerlab destroy --name cldt":
			raw, err := os.ReadFile(r.TopologyPath())
			if err != nil || string(raw) != string(old) {
				t.Fatal("deployed topology overwritten before teardown")
			}
			destroyed = true
		}
		if strings.Contains(command, "containerlab deploy") && !destroyed {
			t.Fatal("deployment preceded old lab teardown")
		}
		return nil, nil, 0, nil
	}
	if err := r.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !destroyed {
		t.Fatal("left old lab nodes behind")
	}
}
