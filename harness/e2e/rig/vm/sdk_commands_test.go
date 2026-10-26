package vm

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/sdkfake"
)

func TestSDKGuestCommandsPreserveNativeRequests(t *testing.T) {
	for _, mode := range []string{"vm", "vm-appliance"} {
		t.Run(mode, func(t *testing.T) { testSDKCommands(t, mode) })
	}
}

func testSDKCommands(t *testing.T, mode string) {
	r := New(lab.Default(), t.TempDir())
	runtime, service := sdkfake.Start(t, r.Kind())
	r.Runtime = runtime
	r.Run = func(context.Context, io.Reader, ...string) ([]byte, []byte, int, error) {
		t.Fatal("SDK guest control fell back to host CLI")
		return nil, nil, 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	node := r.Node("remote1")
	if mode == "vm-appliance" {
		node = r.Node("bastion")
	}
	argv := []string{"tool", "space and ; shell characters"}
	out, err := node.Pipe(ctx, strings.NewReader("input\x00bytes"), argv...)
	if err != nil || string(out) != "output" {
		t.Fatalf("pipe: %q %v", out, err)
	}
	out, err = node.Exec(ctx, "fail")
	var exit *rig.ExitError
	if !errors.As(err, &exit) || exit.Code != 7 || string(out) != "partial" || string(exit.Stderr) != "diagnostic" {
		t.Fatalf("exit: %q %v", out, err)
	}
	if err := node.Put(ctx, strings.NewReader("file\x00bytes"), "/tmp/guest file", 0600); err != nil {
		t.Fatal(err)
	}
	execs, put := service.Requests()
	first := execs[0]
	if first.Node.SessionId != "owned" || first.Node.Node != node.Name() || !reflect.DeepEqual(first.Argv, argv) || string(first.Stdin) != "input\x00bytes" {
		t.Fatalf("request changed: %v", first)
	}
	if put == nil || put.Path != "/tmp/guest file" || put.Mode != 0600 || string(put.Content) != "file\x00bytes" {
		t.Fatalf("put changed: %v", put)
	}
}
