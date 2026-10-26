package vm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/sdkfake"
)

// Only the cluster runs on machines. The routers, the cloud edges and
// the bastion stay containers, as the topology says, and they are
// reached through the lab session as themselves: there is no guest
// inside them to ssh to, and the first bring-up failed with a command
// not found trying.
func TestAnApplianceIsStillAContainer(t *testing.T) {
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir()}
	runtime, service := sdkfake.Start(t, r.Kind())
	r.Runtime = runtime
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.Node("router").Exec(ctx, "true"); err != nil {
		t.Fatal(err)
	}
	execs, _ := service.Requests()
	if len(execs) != 1 || execs[0].Node.Node != "router" || strings.Join(execs[0].Argv, " ") != "true" {
		t.Errorf("an appliance was not reached as itself: %v", execs)
	}

	// And a cluster node still is a machine, reached over serial.
	rec := &recorder{}
	machines := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: rec.run}
	if _, err := machines.Node("remote1").Exec(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(rec.calls[0], " "), "/cldt-guest") {
		t.Errorf("a machine was not reached over serial: %v", rec.calls[0])
	}
}

// An appliance names its interfaces the way the topology does; only a
// guest renames them.
func TestAnApplianceKeepsTheTopologysInterfaceNames(t *testing.T) {
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir()}
	if got := r.Node("router").Interface(0); got != "eth1" {
		t.Errorf("the router calls its first link %q", got)
	}
	if got := r.Node("remote1").Interface(0); got != GuestInterface(0) {
		t.Errorf("a machine calls its first link %q, want %q", got, GuestInterface(0))
	}
}
