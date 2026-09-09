package vm

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// Only the cluster runs on machines. The routers, the cloud edges and
// the bastion stay containers, as the topology says, and they are
// reached the way containers are: there is no guest inside them to
// ssh to, and the first bring-up failed with a command not found
// trying.
func TestAnApplianceIsStillAContainer(t *testing.T) {
	rec := &recorder{}
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: rec.run}

	if _, err := r.Node("router").Exec(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(rec.calls[0], " ")
	if strings.Contains(cmd, "ssh") {
		t.Errorf("an appliance was reached over ssh: %s", cmd)
	}
	if !strings.Contains(cmd, "docker exec clab-cldt-router true") {
		t.Errorf("an appliance was not reached as a container: %s", cmd)
	}

	// And a cluster node still is one.
	rec.calls = nil
	if _, err := r.Node("remote1").Exec(context.Background(), "true"); err != nil {
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
