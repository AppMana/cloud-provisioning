package vm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"github.com/srl-labs/containerlab/core"
	"github.com/srl-labs/containerlab/types"
)

// This tests the product adapter against a real, networkless Linux VM. It is
// not a Kubernetes or product image-build qualification.
func TestLiveSDKGuestCommands(t *testing.T) {
	image := os.Getenv("CLOUD_PROVISIONING_SDK_VM_IMAGE")
	peerImage := os.Getenv("CLOUD_PROVISIONING_SDK_APPLIANCE_IMAGE")
	if image == "" || peerImage == "" {
		t.Skip("requires explicit prepared VM and appliance images, Docker and KVM")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	work, err := os.MkdirTemp("", "cloud-sdk-vm-commands-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retained evidence and control reference: %s", work)
	r := New(lab.Default(), work)
	r.Run = func(context.Context, io.Reader, ...string) ([]byte, []byte, int, error) {
		t.Fatal("guest command bypassed the SDK")
		return nil, nil, 0, nil
	}
	owner := rig.LabcontainersRuntime{}
	if err := owner.Deploy(ctx, work, r.Kind(), "sdk-commands", &core.Config{
		Name: "sdk-commands", Topology: &types.Topology{Nodes: map[string]*types.NodeDefinition{
			"cp":      {Kind: "generic_vm", Image: image, ImagePullPolicy: "Never", NetworkMode: "none"},
			"bastion": {Kind: "linux", Image: peerImage, ImagePullPolicy: "Never", NetworkMode: "none"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	var socket string
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if _, err := owner.Destroy(cleanup, work, r.Kind()); err != nil {
			t.Error("cleanup:", err)
			return
		}
		for socket != "" {
			if _, err := os.Stat(socket); os.IsNotExist(err) {
				break
			}
			select {
			case <-cleanup.Done():
				t.Error("daemon did not exit:", cleanup.Err())
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
	c, session, err := owner.Open(ctx, work, r.Kind())
	if err != nil {
		t.Fatal(err)
	}
	socket = c.Socket()
	t.Logf("session: %s", session.ID())
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	node := r.Node("cp")
	for {
		attempt, stop := context.WithTimeout(ctx, 5*time.Second)
		_, err := node.Exec(attempt, "true")
		stop()
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("guest not ready:", err)
		case <-time.After(2 * time.Second):
		}
	}
	interfaces, err := node.Exec(ctx, "ls", "/sys/class/net")
	if err != nil || strings.TrimSpace(string(interfaces)) != "lo" {
		t.Fatalf("unexpected guest NICs: %q %v", interfaces, err)
	}
	payload := []byte("binary\x00payload\xff\n")
	out, err := node.Pipe(ctx, bytes.NewReader(payload), "cat")
	if err != nil || !bytes.Equal(out, payload) {
		t.Fatalf("stdin changed: %x %v", out, err)
	}
	const path = "/tmp/sdk transfer/nested/file"
	if err := node.Put(ctx, bytes.NewReader(payload), path, 0600); err != nil {
		t.Fatal(err)
	}
	out, err = node.Exec(ctx, "cat", path)
	if err != nil || !bytes.Equal(out, payload) {
		t.Fatalf("file bytes changed: %x %v", out, err)
	}
	out, err = node.Exec(ctx, "stat", "-c", "%a", path)
	if err != nil || strings.TrimSpace(string(out)) != "600" {
		t.Fatalf("file mode: %q %v", out, err)
	}
	out, err = node.Exec(ctx, "sh", "-c", "printf partial; printf diagnostic >&2; exit 7")
	var exit *rig.ExitError
	if !errors.As(err, &exit) || exit.Code != 7 || string(out) != "partial" || string(exit.Stderr) != "diagnostic" {
		t.Fatalf("command failure changed: %q %v", out, err)
	}
	appliance := r.Node("bastion")
	out, err = appliance.Pipe(ctx, bytes.NewReader(payload), "cat")
	if err != nil || !bytes.Equal(out, payload) {
		t.Fatalf("appliance stdin changed: %x %v", out, err)
	}
	if err := appliance.Put(ctx, bytes.NewReader(payload), path, 0600); err != nil {
		t.Fatal(err)
	}
	out, err = appliance.Exec(ctx, "cat", path)
	if err != nil || !bytes.Equal(out, payload) {
		t.Fatalf("appliance file changed: %x %v", out, err)
	}
	out, err = appliance.Exec(ctx, "ls", "/sys/class/net")
	if err != nil || strings.TrimSpace(string(out)) != "lo" {
		t.Fatalf("unexpected appliance NICs: %q %v", out, err)
	}
}
