package vm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

func TestInstanceRetryObservesBootAfterTimeoutAndReconstruction(t *testing.T) {
	state := wrapperObservation{ID: "wrapper-uid"}
	state.State.Running, state.State.StartedAt = true, "initial"
	starts, resets := 0, 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timeout := true
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir()}
	r.Run = func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, []byte, int, error) {
		cmd := strings.Join(args, " ")
		switch {
		case strings.Contains(cmd, "{{json .Id}}"):
			b, _ := json.Marshal(state)
			return b, nil, 0, nil
		case strings.Contains(cmd, "{{.State.Running}}"):
			return []byte(fmt.Sprint(state.State.Running)), nil, 0, nil
		case strings.Contains(cmd, "/cldt-reset-instance"):
			resets++
		case strings.Contains(cmd, "docker kill"):
			state.State.Running = false
		case strings.Contains(cmd, "docker start"):
			starts++
			state.State.Running = true
			state.State.StartedAt = fmt.Sprint(starts)
		case strings.Contains(cmd, "/cldt-guest exec") && strings.HasSuffix(cmd, " true") && timeout:
			cancel()
			return nil, nil, -1, context.DeadlineExceeded
		}
		return nil, nil, 0, nil
	}
	if err := r.ensureKey(); err != nil {
		t.Fatal(err)
	}
	data := rig.BootstrapData{Format: rig.CloudConfig, Value: []byte("#cloud-config\nruncmd: [true]\n")}
	if err := r.Node("remote1").(*Node).BootstrapInstance(ctx, "infra-1", data); err == nil {
		t.Fatal("expected observation timeout")
	}
	if starts != 1 || resets != 1 {
		t.Fatalf("starts=%d resets=%d", starts, resets)
	}
	timeout = false
	// Reconstruct both objects, retaining only durable work-directory state.
	restarted := &Rig{Topology: r.Topology, WorkDir: r.WorkDir, Run: r.Run}
	n := restarted.Node("remote1").(*Node)
	if err := n.BootstrapInstance(context.Background(), "infra-1", data); err != nil {
		t.Fatal(err)
	}
	if starts != 1 || resets != 1 {
		t.Fatal("observer retry relaunched the VM")
	}
	changed := data
	changed.Value = []byte("#cloud-config\nruncmd: [false]\n")
	if err := n.BootstrapInstance(context.Background(), "infra-1", changed); err == nil {
		t.Fatal("accepted changed userdata")
	}
	if err := n.BootstrapInstance(context.Background(), "infra-2", data); err == nil {
		t.Fatal("replaced running predecessor")
	}
	state.State.Running = false
	if err := n.BootstrapInstance(context.Background(), "infra-1", data); err == nil {
		t.Fatal("rebooted stopped instance")
	}
	if err := n.BootstrapInstance(context.Background(), "infra-2", data); err != nil {
		t.Fatal(err)
	}
	if starts != 2 || resets != 2 {
		t.Fatal("new infrastructure UID did not get a fresh disk")
	}
	path := filepath.Join(r.SeedDir("remote1"), "launch.json")
	if err := writeLaunchReceipt(path, launchReceipt{UID: "infra-2", Hash: fmt.Sprintf("%x", sha256.Sum256(data.Value)), WrapperID: state.ID, Phase: "launching"}); err != nil {
		t.Fatal(err)
	}
	if err := n.BootstrapInstance(context.Background(), "infra-2", data); err == nil || !strings.Contains(err.Error(), "outcome unresolved") {
		t.Fatalf("expected unresolved launch, got %v", err)
	}
	if starts != 2 || resets != 2 {
		t.Fatal("uncertain launch was replayed")
	}
}
