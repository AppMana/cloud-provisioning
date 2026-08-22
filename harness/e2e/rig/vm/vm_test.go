package vm

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

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

// The guest is reached through its wrapper and then over loopback,
// never from this host.
//
// It sits behind qemu's usermode NAT with its ports forwarded onto
// the wrapper's own loopback, and this host has no route to it at
// all — which is the point: every route this host had to a node would
// be a path the lab's segments do not explain, and the isolation the
// lab exists to prove would be weaker than it claims.
func TestTheGuestIsReachedThroughItsWrapper(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)

	if _, err := n.Exec(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(rec.calls[0], " ")
	if !strings.HasPrefix(cmd, "docker exec ") {
		t.Errorf("the command does not enter the wrapper: %s", cmd)
	}
	if !strings.Contains(cmd, "clab-cldt-remote1") {
		t.Errorf("the wrong wrapper: %s", cmd)
	}
	if !strings.Contains(cmd, "sysadmin@127.0.0.1") {
		t.Errorf("the guest is not reached over the wrapper's loopback: %s", cmd)
	}
	// A node's work is its root's work, and the login is not root.
	// Every word is quoted, because ssh hands one string to a shell
	// on the far side rather than an argv.
	if !strings.Contains(cmd, "-- sudo 'true'") {
		t.Errorf("the command does not run as root: %s", cmd)
	}
}

// Standard input is attached only when there is some, the same trap
// the container rig closes: a command reading an unattached stdin
// succeeds having read nothing.
func TestInputIsAttachedOnlyWhenThereIsInput(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)

	if _, err := n.Exec(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(rec.calls[0], " "), "docker exec -i") {
		t.Error("a command with no input was run with -i")
	}
	if _, err := n.Pipe(context.Background(), strings.NewReader("body"), "cat"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(rec.calls[1], " "), "docker exec -i") {
		t.Errorf("a command with input was run without -i: %v", rec.calls[1])
	}
	if rec.stdins[1] != "body" {
		t.Errorf("stdin was %q", rec.stdins[1])
	}
}

// Cutting a machine takes its data links and leaves the management
// link alone: taking that away would be taking away the ability to
// observe rather than modelling a pulled cable. A real machine losing
// its data network keeps whatever out-of-band access its operator
// has.
func TestCuttingLeavesTheManagementLinkAlone(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)

	if err := n.Cut(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range rec.calls {
		cmd := strings.Join(call, " ")
		if strings.Contains(cmd, GuestInterface(-1)) {
			t.Errorf("the management interface was taken down: %s", cmd)
		}
		if !strings.Contains(cmd, "'link' 'set'") || !strings.Contains(cmd, "'down'") {
			t.Errorf("unexpected command while cutting: %s", cmd)
		}
	}
	if len(rec.calls) != len(lab.Default().MustNode("remote1").Interfaces) {
		t.Errorf("%d links cut, want one per data link", len(rec.calls))
	}
}

// A machine reads its userdata at first boot, from the seed its
// platform gave it. Handing it to a running machine is not a thing a
// platform does, and a rig that pretended otherwise would be back to
// interpreting the document itself.
func TestUserdataCannotBeHandedToARunningMachine(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)

	err := n.Userdata(context.Background(), []byte("#cloud-config\n"))
	if err == nil {
		t.Fatal("a running machine accepted userdata")
	}
	if !strings.Contains(err.Error(), "first boot") {
		t.Errorf("the error does not say when userdata is read: %v", err)
	}
}
