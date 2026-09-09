package vm

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

func TestBootstrapFailureUsesSerialTransport(t *testing.T) {
	var command string
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: func(_ context.Context, _ io.Reader, argv ...string) ([]byte, []byte, int, error) {
		command = strings.Join(argv, " ")
		return []byte(`{"status":"error"}`), []byte("private userdata"), 1, nil
	}}
	err := r.Node("remote1").(*Node).BootstrapFailure(context.Background())
	if err == nil || strings.Contains(err.Error(), "private userdata") {
		t.Fatalf("terminal failure: %v", err)
	}
	if !strings.Contains(command, "/cldt-guest exec") || !strings.HasSuffix(command, "cloud-init status --format json") {
		t.Fatalf("wrong guest probe: %s", command)
	}
}

func TestBootstrapCompletionUsesSerialTransport(t *testing.T) {
	calls := 0
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: func(_ context.Context, _ io.Reader, argv ...string) ([]byte, []byte, int, error) {
		calls++
		command := strings.Join(argv, " ")
		if !strings.Contains(command, "/cldt-guest exec") {
			t.Fatalf("nonserial command: %s", command)
		}
		if calls == 1 {
			return []byte(`{"status":"done"}`), nil, 0, nil
		}
		if !strings.HasSuffix(command, "test -f /run/cluster-api/bootstrap-success.complete") {
			t.Fatalf("wrong marker command: %s", command)
		}
		return nil, nil, 0, nil
	}}
	done, err := r.Node("remote1").(*Node).BootstrapComplete(context.Background())
	if !done || err != nil || calls != 2 {
		t.Fatalf("done=%v err=%v calls=%d", done, err, calls)
	}
}
