package bringup

import (
	"context"
	"io"
	"io/fs"
	"strconv"
	"sync"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// fakeRig records what each node was asked to do, so that the
// configuration a topology implies can be asserted without a lab that
// would carry it out.
type fakeRig struct {
	mu       sync.Mutex
	commands map[string][][]string
}

func (f *fakeRig) Kind() string                   { return "fake" }
func (f *fakeRig) Up(ctx context.Context) error   { return nil }
func (f *fakeRig) Down(ctx context.Context) error { return nil }
func (f *fakeRig) Node(name string) rig.Node      { return &fakeNode{rig: f, name: name} }

func (f *fakeRig) record(node string, argv []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commands == nil {
		f.commands = map[string][][]string{}
	}
	f.commands[node] = append(f.commands[node], argv)
}

func (f *fakeRig) commandsFor(node string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commands[node]
}

type fakeNode struct {
	rig  *fakeRig
	name string
}

func (n *fakeNode) Name() string { return n.name }

func (n *fakeNode) Exec(ctx context.Context, argv ...string) ([]byte, error) {
	n.rig.record(n.name, argv)
	return nil, nil
}

func (n *fakeNode) Pipe(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	return n.Exec(ctx, argv...)
}

func (n *fakeNode) Put(ctx context.Context, src io.Reader, dst string, mode fs.FileMode) error {
	return nil
}

func (n *fakeNode) Cut(ctx context.Context) error                          { return nil }
func (n *fakeNode) Restore(ctx context.Context) error                      { return nil }
func (n *fakeNode) Kill(ctx context.Context) error                         { return nil }
func (n *fakeNode) Boot(ctx context.Context) error                         { return nil }
func (n *fakeNode) Userdata(ctx context.Context, cloudConfig []byte) error { return nil }

// Interface is what this node calls the lab's nth link. A fake stands
// in for a container, which calls it what the topology does.
func (n *fakeNode) Interface(nth int) string { return "eth" + strconv.Itoa(nth+1) }
