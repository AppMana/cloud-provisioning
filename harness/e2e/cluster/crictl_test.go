package cluster

import (
	"context"
	"errors"
	"io"
	"io/fs"
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

// And a node without one is found, rather than every check failing
// identically later with "command not found" — which reads like a lab
// with no connectivity rather than a lab with no tool.
func TestANodeWithoutTheToolIsNoticed(t *testing.T) {
	r := &toolRig{has: false}
	err := EnsureCRICTL(context.Background(), r, t.TempDir(), []string{"cp"})
	// Fetching may fail on a host with no route out; what matters is
	// that it looked, and did not quietly decide the node was fine.
	if err == nil && r.puts == 0 {
		t.Error("a node with no crictl was passed over, so every probe from it will fail later")
	}
}

type toolRig struct {
	mu    sync.Mutex
	has   bool
	puts  int
	calls [][]string
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
	has := n.rig.has
	n.rig.mu.Unlock()
	if strings.Contains(strings.Join(argv, " "), "command -v crictl") && !has {
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
	n.rig.mu.Unlock()
	return nil
}
func (n *toolNode) Cut(ctx context.Context) error                          { return nil }
func (n *toolNode) Restore(ctx context.Context) error                      { return nil }
func (n *toolNode) Kill(ctx context.Context) error                         { return nil }
func (n *toolNode) Boot(ctx context.Context) error                         { return nil }
func (n *toolNode) Userdata(ctx context.Context, cloudConfig []byte) error { return nil }
func (n *toolNode) Interface(nth int) string                               { return "eth" + strconv.Itoa(nth+1) }
