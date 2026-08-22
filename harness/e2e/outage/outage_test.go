package outage

import (
	"context"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/check"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// A row that starts broken must stop, not proceed. A failure measured
// after the break would be attributed to the break, and the row would
// report the wrong culprit with complete confidence.
func TestABrokenBaselineStopsTheRow(t *testing.T) {
	p := &fakeProber{broken: map[string]bool{"a->b": true}}
	res := Run(context.Background(), Row{Name: "x", Victim: "c", Mode: Cut}, deps(p))

	if res.OK() {
		t.Fatal("a row with a broken baseline passed")
	}
	if !strings.Contains(res.Failed, "baseline") {
		t.Errorf("the failure does not name the baseline: %q", res.Failed)
	}
	if res.Survivors != nil {
		t.Error("the row went on to measure the survivors after the baseline failed")
	}
}

// The victim's own pods go with it, which is allowed. Measuring the
// victim among the survivors would fail every row for the thing the
// row is doing on purpose.
func TestTheVictimIsNotMeasuredAmongTheSurvivors(t *testing.T) {
	p := &fakeProber{}
	// Cut, nothing reaches the victim; restored, it comes back. Which
	// is the whole shape of a passing row.
	p.onCut = func() { p.unreachable = "c" }
	p.onRestore = func() { p.unreachable = "" }

	res := Run(context.Background(), Row{Name: "x", Victim: "c", Mode: Cut}, deps(p))
	if !res.OK() {
		t.Fatalf("a row where only the victim was lost failed: %s", res)
	}
	for _, r := range res.Survivors.Results() {
		if r.From == "c" || r.To == "c" {
			t.Errorf("the victim was measured among the survivors: %v", r)
		}
	}
}

// A cut leaves the machine running: the network goes, the node does
// not. A row that killed the machine would be measuring a different
// claim than the one it says it measures.
func TestACutLeavesTheMachineRunning(t *testing.T) {
	r := &fakeRig{}
	p := &fakeProber{}
	d := deps(p)
	d.Rig = r

	Run(context.Background(), Row{Name: "x", Victim: "c", Mode: Cut}, d)

	if !r.did("c", "cut") || !r.did("c", "restore") {
		t.Errorf("a cut row did not cut and restore: %v", r.calls)
	}
	if r.did("c", "kill") {
		t.Error("a cut row killed the machine, which is a different claim")
	}
}

// A pulled cable takes the node's routes with it, and nothing in the
// kernel puts them back. So a cut row is given the platform's share
// too: a node restored with an address and no gateway is reachable on
// its own segment and nowhere else, which reads exactly like the
// cluster failing to readmit it. Measured on cp, whose every check
// failed for a full fifteen minute window with its address restored
// and its default route gone.
func TestACutRowAlsoGetsThePlatformsShare(t *testing.T) {
	r := &fakeRig{}
	p := &fakeProber{}
	d := deps(p)
	d.Rig = r
	var restored string
	d.Restart = func(ctx context.Context, victim string) error {
		restored = victim
		return nil
	}

	Run(context.Background(), Row{Name: "x", Victim: "c", Mode: Cut}, d)

	if restored != "c" {
		t.Errorf("a cut row never had the platform'''s share restored (%q)", restored)
	}
}

// A reboot takes the machine and gives back only what a platform
// provides, so the caller is given the chance to re-plumb exactly
// that and nothing else.
func TestARebootKillsAndOffersThePlatformsShare(t *testing.T) {
	r := &fakeRig{}
	p := &fakeProber{}
	d := deps(p)
	d.Rig = r
	var restarted string
	d.Restart = func(ctx context.Context, victim string) error {
		restarted = victim
		return nil
	}

	Run(context.Background(), Row{Name: "x", Victim: "c", Mode: Reboot}, d)

	if !r.did("c", "kill") || !r.did("c", "boot") {
		t.Errorf("a reboot row did not kill and boot: %v", r.calls)
	}
	if r.did("c", "cut") {
		t.Error("a reboot row cut the link instead of taking the machine")
	}
	if restarted != "c" {
		t.Errorf("the platform's share was never restored for %q", restarted)
	}
}

// A victim that comes back and leaves the cluster short is the
// failure the third claim exists to catch.
func TestAVictimThatDoesNotComeBackFailsTheRow(t *testing.T) {
	p := &fakeProber{}
	p.onCut = func() { p.unreachable = "c" }
	// It comes back on the network but never rejoins the pod network,
	// which is the shape of a node that returned and was not readmitted.
	p.onRestore = func() { p.unreachable = ""; p.broken = map[string]bool{"a->c": true} }

	res := Run(context.Background(), Row{Name: "x", Victim: "c", Mode: Cut}, deps(p))
	if res.OK() {
		t.Fatal("a row whose victim never recovered passed")
	}
	if !strings.Contains(res.Failed, "return") {
		t.Errorf("the failure does not name the return: %q", res.Failed)
	}
}

func deps(p *fakeProber) Deps {
	return Deps{
		Rig:    &fakeRig{prober: p},
		Prober: p,
		Targets: []check.Target{
			{Node: "a", PodIP: "10.0.0.1", ServiceIP: "10.96.0.1"},
			{Node: "b", PodIP: "10.0.0.2", ServiceIP: "10.96.0.2"},
			{Node: "c", PodIP: "10.0.0.3", ServiceIP: "10.96.0.3"},
		},
		Options:  check.Options{Port: 8080, SkipTransfer: true},
		Converge: 50 * time.Millisecond,
		Down:     time.Millisecond,
	}
}

// fakeProber answers reachability from a table the row's own actions
// change, so a row's sequence can be exercised without a lab.
type fakeProber struct {
	mu          sync.Mutex
	broken      map[string]bool
	unreachable string
	onCut       func()
	onRestore   func()
}

func (f *fakeProber) HTTPGet(ctx context.Context, node, url string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unreachable != "" && (node == f.unreachable || strings.Contains(url, addrOf(f.unreachable))) {
		return nil, errDown
	}
	for pair := range f.broken {
		from, to, _ := strings.Cut(pair, "->")
		if node == from && strings.Contains(url, addrOf(to)) {
			return nil, errDown
		}
	}
	return []byte("ok"), nil
}

func (f *fakeProber) Resolve(ctx context.Context, node, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if node == f.unreachable {
		return errDown
	}
	return nil
}

func addrOf(node string) string {
	switch node {
	case "a":
		return "10.0.0.1"
	case "b":
		return "10.0.0.2"
	default:
		return "10.0.0.3"
	}
}

var errDown = &down{}

type down struct{}

func (*down) Error() string { return "unreachable" }

type fakeRig struct {
	mu     sync.Mutex
	calls  []string
	prober *fakeProber
}

func (f *fakeRig) Kind() string                   { return "fake" }
func (f *fakeRig) Up(ctx context.Context) error   { return nil }
func (f *fakeRig) Down(ctx context.Context) error { return nil }
func (f *fakeRig) Node(name string) rig.Node      { return &fakeNode{rig: f, name: name} }

func (f *fakeRig) record(what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, what)
}

// fire runs one of the prober's hooks, if the row was given a prober
// and that hook was set.
func (f *fakeRig) fire(pick func(*fakeProber) func()) {
	if f.prober == nil {
		return
	}
	if hook := pick(f.prober); hook != nil {
		hook()
	}
}

func (f *fakeRig) did(node, what string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == node+":"+what {
			return true
		}
	}
	return false
}

type fakeNode struct {
	rig  *fakeRig
	name string
}

func (n *fakeNode) Name() string { return n.name }
func (n *fakeNode) Exec(ctx context.Context, argv ...string) ([]byte, error) {
	return nil, nil
}
func (n *fakeNode) Pipe(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	return nil, nil
}
func (n *fakeNode) Put(ctx context.Context, src io.Reader, dst string, mode fs.FileMode) error {
	return nil
}

// The rig's actions change what the prober answers, so a row is
// exercised against a world its own steps alter rather than a fixed
// table.
func (n *fakeNode) Cut(ctx context.Context) error {
	n.rig.record(n.name + ":cut")
	n.rig.fire(func(p *fakeProber) func() { return p.onCut })
	return nil
}

func (n *fakeNode) Restore(ctx context.Context) error {
	n.rig.record(n.name + ":restore")
	n.rig.fire(func(p *fakeProber) func() { return p.onRestore })
	return nil
}

func (n *fakeNode) Kill(ctx context.Context) error {
	n.rig.record(n.name + ":kill")
	n.rig.fire(func(p *fakeProber) func() { return p.onCut })
	return nil
}

func (n *fakeNode) Boot(ctx context.Context) error {
	n.rig.record(n.name + ":boot")
	n.rig.fire(func(p *fakeProber) func() { return p.onRestore })
	return nil
}
func (n *fakeNode) Userdata(ctx context.Context, cloudConfig []byte) error {
	return nil
}

// Interface is what this node calls the lab's nth link. A fake stands
// in for a container, which calls it what the topology does.
func (n *fakeNode) Interface(nth int) string { return "eth" + strconv.Itoa(nth+1) }
