package container

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// recorder is a Runner that answers nothing and remembers everything,
// so what the rig asks the host to do can be asserted without a host
// that would do it.
type recorder struct {
	calls  [][]string
	stdins []string
	code   int
}

func (r *recorder) run(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, []byte, int, error) {
	r.calls = append(r.calls, argv)
	body := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		body = string(b)
	}
	r.stdins = append(r.stdins, body)
	return nil, nil, r.code, nil
}

func testNode(t *testing.T, rec *recorder) *Node {
	t.Helper()
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: rec.run}
	return r.Node("remote1").(*Node)
}

// The trap this interface exists to close: a command with something
// on standard input must be run with -i. Without it the input is
// never attached, and kubectl apply reading an empty stdin applies
// nothing and reports success — a green row that installed nothing.
func TestInputIsAttachedOnlyWhenThereIsInput(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)

	if _, err := n.Exec(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	if has(rec.calls[0], "-i") {
		t.Error("a command with no input was run with -i")
	}

	if _, err := n.Pipe(context.Background(), strings.NewReader("manifest"), "kubectl", "apply", "-f", "-"); err != nil {
		t.Fatal(err)
	}
	if !has(rec.calls[1], "-i") {
		t.Errorf("a command with input was run without -i: %v", rec.calls[1])
	}
	if rec.stdins[1] != "manifest" {
		t.Errorf("stdin was %q, want the manifest", rec.stdins[1])
	}
}

// Words are passed through, never assembled into a shell string. A
// value with a space in it becoming two words is the class of bug
// that makes a harness lie about what it ran.
func TestArgumentsAreNotReassembledIntoAShell(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)

	if _, err := n.Exec(context.Background(), "sh", "-c", "echo one two"); err != nil {
		t.Fatal(err)
	}
	got := rec.calls[0]
	if got[len(got)-1] != "echo one two" {
		t.Errorf("the last word is %q: the command was split", got[len(got)-1])
	}
	if got[0] != "docker" || got[1] != "exec" {
		t.Errorf("unexpected command shape %v", got)
	}
}

// The topology names a node cp; docker names it clab-cldt-cp. Every
// caller uses the topology's name, so the mapping lives in one place.
func TestTheTopologyNameIsWhatCallersUse(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)
	if n.Name() != "remote1" {
		t.Errorf("Name() = %q, want the topology's name", n.Name())
	}
	if n.Container() != "clab-cldt-remote1" {
		t.Errorf("Container() = %q", n.Container())
	}
}

// A non-zero exit is an error carrying what the command said, because
// a harness that reports only a status makes every failure a second
// investigation.
func TestAFailedCommandCarriesItsOutput(t *testing.T) {
	rec := &recorder{code: 2}
	n := testNode(t, rec)
	_, err := n.Exec(context.Background(), "false")
	if err == nil {
		t.Fatal("a command that exited 2 returned no error")
	}
	if !strings.Contains(err.Error(), "remote1") || !strings.Contains(err.Error(), "exited 2") {
		t.Errorf("the error does not say which node or what happened: %v", err)
	}
}

// Writing a file creates its parent first. cloud-init does; a shell
// redirect does not, and /etc/wg-dialer does not exist until the
// first file lands in it.
func TestWritingAFileCreatesItsDirectory(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)

	if err := n.Put(context.Background(), strings.NewReader("x"), "/etc/wg-dialer/peers.json", 0o600); err != nil {
		t.Fatal(err)
	}
	if !has(rec.calls[0], "mkdir") || !has(rec.calls[0], "/etc/wg-dialer") {
		t.Errorf("the parent directory was not created: %v", rec.calls[0])
	}
	// Mode is set in octal. Formatted as decimal, 0600 becomes "384",
	// which chmod reads as 0o384 — a different, wrong mode.
	last := rec.calls[len(rec.calls)-1]
	if !has(last, "chmod") || !has(last, "600") {
		t.Errorf("the mode was not set in octal: %v", last)
	}
}

// Cutting a node takes its links down and leaves it running: the
// machine keeps working against a network that is gone, which is what
// a pulled cable looks like from everywhere else.
func TestCuttingANodeLeavesItRunning(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)

	if err := n.Cut(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range rec.calls {
		if has(call, "kill") || has(call, "stop") {
			t.Errorf("cutting the link stopped the machine: %v", call)
		}
		if !has(call, "down") {
			t.Errorf("expected a link to go down: %v", call)
		}
	}
	if len(rec.calls) != len(lab.Default().MustNode("remote1").Interfaces) {
		t.Errorf("%d links taken down, want one per interface", len(rec.calls))
	}
}

// Restoring re-adds the address. A link coming back up does not
// necessarily bring one with it, and a node with an interface and no
// address looks alive and reaches nothing.
func TestRestoringPutsTheAddressBack(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)

	if err := n.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sawAddress bool
	for _, call := range rec.calls {
		if has(call, "addr") && has(call, "203.0.113.10/24") {
			sawAddress = true
		}
	}
	if !sawAddress {
		t.Errorf("the address was not restored: %v", rec.calls)
	}
}

// Userdata is applied in order: every file, then every command. A
// command that reads a file the document also writes is the ordinary
// case, and running commands first would break it.
func TestUserdataWritesBeforeItRuns(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)

	doc := []byte("#cloud-config\n" +
		"write_files:\n" +
		"  - path: /etc/wg-dialer/machine-name\n    content: remote1\n" +
		"runcmd:\n" +
		"  - [systemctl, enable, --now, wg-dialer]\n")
	if err := n.Userdata(context.Background(), doc); err != nil {
		t.Fatal(err)
	}

	writeAt, runAt := -1, -1
	for i, call := range rec.calls {
		if has(call, "cat > '/etc/wg-dialer/machine-name'") {
			writeAt = i
		}
		if has(call, "systemctl") {
			runAt = i
		}
	}
	if writeAt < 0 || runAt < 0 {
		t.Fatalf("the document was not applied: %v", rec.calls)
	}
	if writeAt > runAt {
		t.Error("a command ran before the file it may need was written")
	}
}

func has(argv []string, want string) bool {
	for _, a := range argv {
		if a == want || strings.Contains(a, want) {
			return true
		}
	}
	return false
}

// The container rig cannot satisfy kubeadm's preflight, because
// preflight inspects the kernel it runs on and in a container that is
// this host's. So the command is relaxed — and the relaxation is
// reported, which is the whole difference from what it replaces: the
// shell harness rewrote the rendered command with a string
// substitution and said nothing, so every kubeadm row ran something
// the product had not rendered and no reader could tell.
func TestPreflightIsRelaxedAndSaidOutLoud(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)

	doc := []byte("#cloud-config\n" +
		"write_files:\n  - path: /tmp/x\n    content: x\n" +
		"runcmd:\n" +
		"  - |\n    kubeadm join 127.0.0.1:7445 \\\n      --token a.b\n")
	if err := n.Userdata(context.Background(), doc); err != nil {
		t.Fatal(err)
	}

	var joined string
	for _, call := range rec.calls {
		if has(call, "kubeadm join") {
			joined = strings.Join(call, " ")
		}
	}
	if joined == "" {
		t.Fatal("the join never ran")
	}
	if !strings.Contains(joined, "--ignore-preflight-errors=all") {
		t.Error("preflight was not relaxed, so the join fails on a kernel the node does not own")
	}
	if len(n.Accommodations()) != 1 {
		t.Fatalf("%d accommodations reported, want the one that was made: %v", len(n.Accommodations()), n.Accommodations())
	}
	if !strings.Contains(n.Accommodations()[0], "kernel") {
		t.Errorf("the accommodation does not say why it was needed: %q", n.Accommodations()[0])
	}
}

// A document that needs no accommodation reports none, so the list
// means something when it is not empty.
func TestADocumentNeedingNothingReportsNothing(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)

	doc := []byte("#cloud-config\nwrite_files:\n  - path: /tmp/x\n    content: x\n" +
		"runcmd:\n  - [systemctl, enable, --now, wg-dialer]\n")
	if err := n.Userdata(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if len(n.Accommodations()) != 0 {
		t.Errorf("accommodations reported for a document that needed none: %v", n.Accommodations())
	}
}
