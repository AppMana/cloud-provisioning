package container

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/sdkfake"
	labv1 "github.com/appmana/labcontainers/api/v1"
)

// recorder is a Runner that answers nothing and remembers everything,
// so what the view asks the host to do can be asserted without a host
// that would do it.
type recorder struct {
	calls [][]string
}

func (r *recorder) run(_ context.Context, _ io.Reader, argv ...string) ([]byte, []byte, int, error) {
	r.calls = append(r.calls, argv)
	return nil, nil, 0, nil
}

func testNode(t *testing.T, name string) (*Node, *sdkfake.Server, *recorder) {
	t.Helper()
	runtime, service := sdkfake.Start(t, "vm")
	rec := &recorder{}
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: rec.run, Runtime: runtime, RuntimeKind: "vm"}
	return r.Node(name).(*Node), service, rec
}

func argvs(execs []*labv1.ExecRequest) [][]string {
	var out [][]string
	for _, e := range execs {
		out = append(out, e.Argv)
	}
	return out
}

func TestTheTopologyNameIsWhatCallersUse(t *testing.T) {
	n, _, _ := testNode(t, "router")
	if n.Name() != "router" {
		t.Errorf("Name() = %q, want the topology's name", n.Name())
	}
	if n.Container() != "clab-cldt-router" {
		t.Errorf("Container() = %q", n.Container())
	}
}

// Cutting an appliance takes its links down through the lab session and
// leaves it running: the router keeps working against a network that is
// gone, which is what a pulled cable looks like from everywhere else.
func TestCuttingAnApplianceLeavesItRunning(t *testing.T) {
	n, service, rec := testNode(t, "router")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.Cut(ctx); err != nil {
		t.Fatal(err)
	}
	execs, _ := service.Requests()
	if len(execs) != len(lab.Default().MustNode("router").Interfaces) {
		t.Errorf("%d links taken down, want one per interface", len(execs))
	}
	for _, argv := range argvs(execs) {
		if !slices.Contains(argv, "down") {
			t.Errorf("expected a link to go down: %v", argv)
		}
	}
	if len(rec.calls) != 0 {
		t.Errorf("cutting the link touched the container: %v", rec.calls)
	}
}

// Restoring re-adds each address. A link coming back up does not
// necessarily bring one with it, and an appliance with an interface and no
// address looks alive and routes nothing.
func TestRestoringPutsTheAddressesBack(t *testing.T) {
	n, service, _ := testNode(t, "router")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	execs, _ := service.Requests()
	for _, i := range lab.Default().MustNode("router").Interfaces {
		if i.Address == "" {
			continue
		}
		found := slices.ContainsFunc(argvs(execs), func(argv []string) bool {
			return slices.Contains(argv, "addr") && slices.Contains(argv, i.Address)
		})
		if !found {
			t.Errorf("address %s was not restored: %v", i.Address, argvs(execs))
		}
	}
}

// Killing and booting act on the container itself, not through the guest.
func TestKillAndBootUseTheContainer(t *testing.T) {
	n, service, rec := testNode(t, "router")
	if err := n.Kill(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := n.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"docker", "kill", "clab-cldt-router"}, {"docker", "start", "clab-cldt-router"}}
	if !slices.EqualFunc(rec.calls, want, slices.Equal) {
		t.Errorf("calls = %v, want %v", rec.calls, want)
	}
	if execs, _ := service.Requests(); len(execs) != 0 {
		t.Errorf("stopping the container ran guest commands: %v", argvs(execs))
	}
}

// Without the lab's session there is no owned path to an appliance, and
// the view must say so rather than fall back to an unowned container.
func TestAnApplianceWithoutItsLabSessionIsRefused(t *testing.T) {
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: (&recorder{}).run}
	_, err := r.Node("router").Exec(context.Background(), "true")
	if err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("err = %v, want a refusal naming the session", err)
	}
}

func TestAppliancesTakeNoUserdata(t *testing.T) {
	n, _, _ := testNode(t, "router")
	if err := n.Userdata(context.Background(), []byte("#cloud-config\n")); err == nil {
		t.Fatal("an appliance accepted userdata")
	}
}
