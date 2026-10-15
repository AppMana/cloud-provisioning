package cluster

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// A node that already has the tool is left alone, which is every node
// on the container rig. Nothing is fetched either: the check runs
// where the lab runs, and a run that reached out for a tool nobody
// needed would fail on a host with no route out.
func TestANodeThatHasTheToolIsLeftAlone(t *testing.T) {
	r := &toolRig{has: true}
	if err := EnsureCRICTL(context.Background(), r, t.TempDir(), []string{"cp", "w1"}); err != nil {
		t.Fatal(err)
	}
	if r.puts != 0 {
		t.Errorf("%d nodes were given a tool they already had", r.puts)
	}
	for _, argv := range r.calls {
		if strings.Contains(strings.Join(argv, " "), "chmod") {
			t.Error("a node that needed nothing was written to anyway")
		}
	}
}

func TestMissingToolsUseNativeArchitecture(t *testing.T) {
	work := t.TempDir()
	for arch, machine := range map[string]uint16{"amd64": 62, "arm64": 183} {
		body := make([]byte, 64)
		copy(body, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
		binary.LittleEndian.PutUint16(body[18:], machine)
		binary.LittleEndian.PutUint32(body[20:], 1)
		if err := os.WriteFile(filepath.Join(work, "crictl-"+CRICTLVersion+"-linux-"+arch), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := &toolRig{architectures: map[string]string{"cp": "Linux x86_64", "remote": "Linux aarch64"}, installed: map[string][]byte{}}
	if err := EnsureCRICTL(context.Background(), r, work, []string{"cp", "remote"}); err != nil {
		t.Fatal(err)
	}
	for node, want := range map[string]uint16{"cp": 62, "remote": 183} {
		if got := binary.LittleEndian.Uint16(r.installed[node][18:]); got != want {
			t.Fatalf("%s got machine %d want %d", node, got, want)
		}
	}
}

func TestUnsupportedPlatformDoesNotInstall(t *testing.T) {
	r := &toolRig{architectures: map[string]string{"cp": "Linux riscv64"}}
	if err := EnsureCRICTL(context.Background(), r, t.TempDir(), []string{"cp"}); err == nil || r.puts != 0 {
		t.Fatal("unsupported platform accepted")
	}
}

type toolRig struct {
	mu            sync.Mutex
	has           bool
	puts          int
	calls         [][]string
	architectures map[string]string
	installed     map[string][]byte
}

func (r *toolRig) Kind() string                   { return "tool" }
func (r *toolRig) Up(ctx context.Context) error   { return nil }
func (r *toolRig) Down(ctx context.Context) error { return nil }
func (r *toolRig) Node(name string) rig.Node      { return &toolNode{rig: r, name: name} }

type toolNode struct {
	rig  *toolRig
	name string
}

func (n *toolNode) Name() string { return n.name }

func (n *toolNode) Exec(ctx context.Context, argv ...string) ([]byte, error) {
	n.rig.mu.Lock()
	n.rig.calls = append(n.rig.calls, argv)
	has := n.rig.has || len(n.rig.installed[n.name]) > 0
	if strings.Join(argv, " ") == "uname -sm" {
		arch := n.rig.architectures[n.name]
		if arch == "" {
			arch = "Linux x86_64"
		}
		n.rig.mu.Unlock()
		return []byte(arch), nil
	}
	n.rig.mu.Unlock()
	if strings.Contains(strings.Join(argv, " "), "crictl --version") && !has {
		return nil, errors.New("not found")
	}
	return nil, nil
}

func (n *toolNode) Pipe(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	return n.Exec(ctx, argv...)
}
func (n *toolNode) Put(ctx context.Context, src io.Reader, dst string, mode fs.FileMode) error {
	n.rig.mu.Lock()
	n.rig.puts++
	if n.rig.installed == nil {
		n.rig.installed = map[string][]byte{}
	}
	body, err := io.ReadAll(src)
	if err != nil {
		n.rig.mu.Unlock()
		return err
	}
	n.rig.installed[n.name] = body
	n.rig.mu.Unlock()
	return nil
}
func (n *toolNode) Cut(ctx context.Context) error                          { return nil }
func (n *toolNode) Restore(ctx context.Context) error                      { return nil }
func (n *toolNode) Kill(ctx context.Context) error                         { return nil }
func (n *toolNode) Boot(ctx context.Context) error                         { return nil }
func (n *toolNode) Userdata(ctx context.Context, cloudConfig []byte) error { return nil }
func (n *toolNode) Interface(nth int) string                               { return "eth" + strconv.Itoa(nth+1) }
