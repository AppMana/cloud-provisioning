package vm

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

type recorder struct {
	calls  [][]string
	stdins []string
	code   int
	// crashLoop makes the wrapper look like one whose launcher keeps
	// exiting, which is what docker reports while a restart policy
	// keeps trying.
	crashLoop bool
}

func (r *recorder) run(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, []byte, int, error) {
	r.calls = append(r.calls, argv)
	body := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		body = string(b)
	}
	r.stdins = append(r.stdins, body)
	if r.crashLoop {
		switch {
		case len(argv) > 1 && argv[1] == "inspect":
			return []byte("restarting 22\n"), nil, 0, nil
		case len(argv) > 1 && argv[1] == "logs":
			return []byte("UnicodeEncodeError: 'ascii' codec can't encode character"), nil, 0, nil
		default:
			return nil, []byte("Container is restarting, wait until the container is running"), 1, nil
		}
	}
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
	if !strings.Contains(cmd, Guest+"@127.0.0.1") {
		t.Errorf("the guest is not reached over the wrapper's loopback: %s", cmd)
	}
	// A node's work is its root's work, and the login is root — which
	// is who cloud-init gives the keys that arrive as metadata. Every
	// word is still quoted, because ssh hands one string to a shell on
	// the far side rather than an argv.
	if !strings.Contains(cmd, "-- 'true'") {
		t.Errorf("the command is not passed as quoted words: %s", cmd)
	}
	if strings.Contains(cmd, "sudo") {
		t.Errorf("the command escalates, when the login already is root: %s", cmd)
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

	var discarded, started, plumbed int
	for _, call := range rec.calls {
		cmd := strings.Join(call, " ")
		switch {
		case strings.Contains(cmd, "-overlay.qcow2"):
			discarded++
		case strings.Contains(cmd, "docker start"):
			started++
		case strings.Contains(cmd, "veth create"):
			plumbed++
		}
	}
	if discarded == 0 {
		t.Error("the instance kept its disk, so it came back past its first boot having read nothing")
	}
	if started == 0 {
		t.Error("the instance was never started")
	}
	// Its links too: a restarted wrapper gets a new network namespace
	// and loses every one of them, and the launcher will not start
	// qemu until the management link is back.
	if plumbed <= len(lab.Default().MustNode("remote1").Interfaces) {
		t.Errorf("%d links restored, want the lab's links and the management one", plumbed)
	}

	// The disk has to go before the instance is stopped, because
	// afterwards there is nothing running to remove it in.
	var discardedAt, killedAt = -1, -1
	for i, call := range rec.calls {
		cmd := strings.Join(call, " ")
		if discardedAt < 0 && strings.Contains(cmd, "-overlay.qcow2") {
			discardedAt = i
		}
		if killedAt < 0 && strings.Contains(cmd, "docker kill") {
			killedAt = i
		}
	}
	if discardedAt < 0 || killedAt < 0 || discardedAt > killedAt {
		t.Errorf("the disk was discarded at %d and the instance stopped at %d", discardedAt, killedAt)
	}
}

// The management link is restored along with the lab's own. The
// launcher waits for it before it starts qemu, so a machine booted
// without it does not come back slowly — it hangs.
func TestTheManagementLinkIsRestoredToo(t *testing.T) {
	rec := &recorder{}
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: rec.run}
	n := r.Node("remote1").(*Node)

	if err := n.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, call := range rec.calls {
		cmd := strings.Join(call, " ")
		if strings.Contains(cmd, "veth create") && strings.Contains(cmd, lab.ManagementBridge("remote1")) {
			found = true
		}
	}
	if !found {
		t.Error("the machine was booted with no management link, which the launcher waits for forever")
	}
}

// A wrapper that cannot start is not a machine that is booting
// slowly. Its launcher exits, its supervisor starts it again, and it
// exits again; waiting the full boot timeout on that reports a
// twelve-minute delay instead of the crash that caused it, and buries
// the launcher's own explanation.
func TestAWrapperThatCannotStartIsNotASlowBoot(t *testing.T) {
	rec := &recorder{crashLoop: true}
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: rec.run}

	start := time.Now()
	err := r.waitForNode(context.Background(), "remote1", 10*time.Minute)
	if err == nil {
		t.Fatal("a crash-looping wrapper was reported as reachable")
	}
	if time.Since(start) > 30*time.Second {
		t.Error("the wait sat out the whole boot timeout on a machine that was never starting")
	}
	if !strings.Contains(err.Error(), "failed to start") {
		t.Errorf("the failure does not say the wrapper never ran: %v", err)
	}
	// The launcher's own words, so the next step is reading them
	// rather than going to find them.
	if !strings.Contains(err.Error(), "UnicodeEncodeError") {
		t.Errorf("the launcher's output was not carried out with the failure: %v", err)
	}
}
