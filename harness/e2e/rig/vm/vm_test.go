package vm

import (
	"context"
	"io"
	"os"
	"path/filepath"
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

// A machine reads its userdata at its first boot, so giving one
// userdata means giving it a first boot: the bytes go into its seed
// and the instance is replaced.
//
// Replaced, not restarted. vrnetlab creates the guest's overlay disk
// only when none exists, so a restarted wrapper brings the same
// instance back past its first boot, having read nothing — the run
// would then report a bootstrap that succeeded and a node that never
// joined.
func TestUserdataLaunchesTheMachineRatherThanRestartingIt(t *testing.T) {
	rec := &recorder{}
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: rec.run}
	if err := r.ensureKey(); err != nil {
		t.Fatal(err)
	}
	n := r.Node("remote1").(*Node)

	const doc = "#cloud-config\nruncmd: [true]\n"
	if err := n.Userdata(context.Background(), []byte(doc)); err != nil {
		t.Fatal(err)
	}

	seeded, err := os.ReadFile(filepath.Join(r.SeedDir("remote1"), "extra-userdata.yaml"))
	if err != nil {
		t.Fatalf("the userdata never reached the seed: %v", err)
	}
	if string(seeded) != doc {
		t.Errorf("the seed carries %q, not the document the product rendered", seeded)
	}

	var removed, deployed, restarted int
	for _, call := range rec.calls {
		cmd := strings.Join(call, " ")
		switch {
		case strings.Contains(cmd, "docker rm"):
			removed++
		case strings.Contains(cmd, "containerlab deploy"):
			deployed++
		case strings.Contains(cmd, "docker restart"), strings.Contains(cmd, "docker start"):
			restarted++
		}
	}
	if removed == 0 || deployed == 0 {
		t.Errorf("the instance was not replaced: removed %d, deployed %d", removed, deployed)
	}
	if restarted > 0 {
		t.Error("the instance was restarted, so the guest came back past its first boot and read nothing")
	}
	// The seed has to be written before the instance is launched: the
	// wrapper builds the cloud-init ISO as it starts, from whatever is
	// there then.
	for i, call := range rec.calls {
		if strings.Contains(strings.Join(call, " "), "containerlab deploy") {
			if i == 0 {
				t.Error("the instance was launched before its seed was written")
			}
			break
		}
	}
}
