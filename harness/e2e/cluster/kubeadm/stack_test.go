package kubeadm

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

// A node image is left alone. kindest/node is one, so the container
// rig fetches nothing and writes nothing, and its rows are unchanged
// by any of this.
func TestANodeImageIsLeftAlone(t *testing.T) {
	r := &stackRig{hasKubeadm: true}
	if err := EnsureStack(context.Background(), r, t.TempDir(), []string{"cp", "w1"}); err != nil {
		t.Fatal(err)
	}
	if r.puts != 0 {
		t.Errorf("%d files were written onto a node that was already a node", r.puts)
	}
}

// And a cloud image is noticed, rather than the row failing later
// inside the builder with "ctr: command not found", which reads as a
// broken lab rather than an empty one.
func TestACloudImageIsNoticed(t *testing.T) {
	r := &stackRig{hasKubeadm: false}
	err := EnsureStack(context.Background(), r, t.TempDir(), []string{"cp"})
	// Fetching needs a route out, which a test host may not have;
	// what matters is that it looked rather than passing the node
	// over.
	if err == nil && r.puts == 0 {
		t.Error("a node with no kubeadm was passed over, so the builder will fail on it later")
	}
}

type stackRig struct {
	mu         sync.Mutex
	hasKubeadm bool
	puts       int
}

func (r *stackRig) Kind() string                   { return "stack" }
func (r *stackRig) Up(ctx context.Context) error   { return nil }
func (r *stackRig) Down(ctx context.Context) error { return nil }
func (r *stackRig) Node(name string) rig.Node      { return &stackNode{rig: r, name: name} }

type stackNode struct {
	rig  *stackRig
	name string
}

func (n *stackNode) Name() string { return n.name }

func (n *stackNode) Exec(ctx context.Context, argv ...string) ([]byte, error) {
	if strings.Contains(strings.Join(argv, " "), "command -v kubeadm") && !n.rig.hasKubeadm {
		return nil, errors.New("not found")
	}
	return nil, nil
}

func (n *stackNode) Pipe(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	return n.Exec(ctx, argv...)
}
func (n *stackNode) Put(ctx context.Context, src io.Reader, dst string, mode fs.FileMode) error {
	n.rig.mu.Lock()
	n.rig.puts++
	n.rig.mu.Unlock()
	return nil
}
func (n *stackNode) Cut(ctx context.Context) error                          { return nil }
func (n *stackNode) Restore(ctx context.Context) error                      { return nil }
func (n *stackNode) Kill(ctx context.Context) error                         { return nil }
func (n *stackNode) Boot(ctx context.Context) error                         { return nil }
func (n *stackNode) Userdata(ctx context.Context, cloudConfig []byte) error { return nil }
func (n *stackNode) Interface(nth int) string                               { return "eth" + strconv.Itoa(nth+1) }
