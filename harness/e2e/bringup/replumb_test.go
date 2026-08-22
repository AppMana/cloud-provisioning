package bringup

import (
	"context"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// A machine cannot answer until it has booted, and it cannot boot
// until it has its links. So the links are restored first, from the
// host, and only then is the node expected to answer.
//
// Written the other way round — wait for the node, then give it its
// NIC — a reboot row on machines waits for a guest that is waiting
// for the interfaces the wait is holding up, and the row expires
// having proved nothing.
func TestLinksAreRestoredBeforeTheNodeIsExpectedToAnswer(t *testing.T) {
	topo := lab.Default()
	h := &replumbHost{gone: map[string]bool{}}
	for _, i := range topo.MustNode("remote1").Interfaces {
		h.gone[lab.EndpointName(i.Segment, topo.MustNode("remote1"))] = true
	}
	r := &bootingRig{host: h}

	if err := Replumb(context.Background(), topo, r, h, "remote1"); err != nil {
		t.Fatal(err)
	}
	if r.answeredBeforeLinks {
		t.Error("the node was asked to answer before it had the links it needs to boot")
	}
}

// A machine whose cable was pulled still has its NIC. Rebuilding it
// would take away an interface the node is still using, which is more
// than a platform does and would turn an outage row into a rebuild.
func TestAPluggedCableIsNotRebuilt(t *testing.T) {
	topo := lab.Default()
	h := &replumbHost{gone: map[string]bool{}} // every endpoint present
	r := &bootingRig{host: h}

	if err := Replumb(context.Background(), topo, r, h, "remote1"); err != nil {
		t.Fatal(err)
	}
	for _, call := range h.calls {
		if strings.Contains(strings.Join(call, " "), "veth create") {
			t.Errorf("a link that was still there was rebuilt: %v", call)
		}
	}
}

// The question is asked of the host, not of the node. A machine's
// kernel names interfaces for the bus it finds them on, so asking it
// about the topology's name for a link never finds one that is there.
func TestTheLinkIsLookedForWhereThePlatformCanSeeIt(t *testing.T) {
	topo := lab.Default()
	h := &replumbHost{gone: map[string]bool{}}
	r := &bootingRig{host: h}

	if err := Replumb(context.Background(), topo, r, h, "remote1"); err != nil {
		t.Fatal(err)
	}
	var asked bool
	for _, call := range h.calls {
		if strings.HasPrefix(strings.Join(call, " "), "ip link show") {
			asked = true
		}
	}
	if !asked {
		t.Error("the host was never asked whether the link is there")
	}
	for _, call := range r.commands {
		if strings.Contains(strings.Join(call, " "), "link show eth") {
			t.Errorf("the node was asked about a link by the topology's name: %v", call)
		}
	}
}

type replumbHost struct {
	mu    sync.Mutex
	calls [][]string
	gone  map[string]bool
}

func (h *replumbHost) Run(ctx context.Context, argv ...string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, argv)
	if len(argv) >= 4 && argv[0] == "ip" && argv[1] == "link" && argv[2] == "show" {
		if h.gone[argv[3]] {
			return nil, errUnreachable
		}
	}
	if len(argv) >= 4 && strings.Join(argv[:4], " ") == "sudo containerlab tools veth" {
		// Whatever it plumbed is now there.
		for name := range h.gone {
			for _, w := range argv {
				if strings.HasSuffix(w, name) {
					h.gone[name] = false
				}
			}
		}
	}
	return nil, nil
}

func (h *replumbHost) InNamespace(ctx context.Context, node string, argv ...string) ([]byte, error) {
	return nil, nil
}

func (h *replumbHost) linksMissing() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, gone := range h.gone {
		if gone {
			return true
		}
	}
	return false
}

// bootingRig is a rig whose nodes cannot answer until their links
// exist, which is what a machine is.
type bootingRig struct {
	mu                  sync.Mutex
	host                *replumbHost
	commands            [][]string
	answeredBeforeLinks bool
}

func (f *bootingRig) Kind() string                   { return "booting" }
func (f *bootingRig) Up(ctx context.Context) error   { return nil }
func (f *bootingRig) Down(ctx context.Context) error { return nil }
func (f *bootingRig) Node(name string) rig.Node      { return &bootingNode{rig: f, name: name} }

type bootingNode struct {
	rig  *bootingRig
	name string
}

func (n *bootingNode) Name() string { return n.name }

func (n *bootingNode) Exec(ctx context.Context, argv ...string) ([]byte, error) {
	n.rig.mu.Lock()
	n.rig.commands = append(n.rig.commands, argv)
	n.rig.mu.Unlock()
	if n.rig.host.linksMissing() {
		n.rig.mu.Lock()
		n.rig.answeredBeforeLinks = true
		n.rig.mu.Unlock()
		return nil, errUnreachable
	}
	return nil, nil
}

func (n *bootingNode) Pipe(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	return n.Exec(ctx, argv...)
}
func (n *bootingNode) Put(ctx context.Context, src io.Reader, dst string, mode fs.FileMode) error {
	return nil
}
func (n *bootingNode) Cut(ctx context.Context) error                          { return nil }
func (n *bootingNode) Restore(ctx context.Context) error                      { return nil }
func (n *bootingNode) Kill(ctx context.Context) error                         { return nil }
func (n *bootingNode) Boot(ctx context.Context) error                         { return nil }
func (n *bootingNode) Userdata(ctx context.Context, cloudConfig []byte) error { return nil }
func (n *bootingNode) Interface(nth int) string                               { return "ens" + strconv.Itoa(nth+2) }
