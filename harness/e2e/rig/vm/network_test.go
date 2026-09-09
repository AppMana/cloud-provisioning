package vm

import (
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// A machine is addressed on the segment its own bootstrap uses,
// before that bootstrap runs.
func TestAMachineIsAddressedByItsPlatform(t *testing.T) {
	topo := lab.Default()
	remote := topo.MustNode("remote1")

	cfg, err := NetworkConfig(remote)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, remote.Interfaces[0].Address) {
		t.Errorf("remote1 boots with no address on its own segment:\n%s", cfg)
	}
	// Its kernel's name for the link, not the topology's: a
	// configuration naming eth1 addresses nothing and says so nowhere.
	if !strings.Contains(cfg, GuestInterface(0)+":") {
		t.Errorf("the configuration names a link the guest does not have:\n%s", cfg)
	}
	if strings.Contains(cfg, remote.Interfaces[0].Name+":") {
		t.Errorf("the configuration uses the topology's name for the link:\n%s", cfg)
	}
}

// Every cluster node can be given one, and each leaves by its own
// segment's edge.
func TestEveryMachineLeavesByItsOwnEdge(t *testing.T) {
	topo := lab.Default()
	for _, n := range topo.Nodes {
		if !n.IsClusterNode() {
			continue
		}
		cfg, err := NetworkConfig(n)
		if err != nil {
			t.Fatalf("%s: %v", n.Name, err)
		}
		via, _ := lab.Gateway(n.Interfaces[0].Segment)
		if !strings.Contains(cfg, "via: "+via) {
			t.Errorf("%s does not leave by %s:\n%s", n.Name, via, cfg)
		}
	}
}

// The management path is not a way out of the lab. A machine that
// could leave by it would leave by a path no router in the topology
// explains, and the isolation proof would be proving nothing.
func TestTheManagementPathIsNotAWayOut(t *testing.T) {
	cfg, err := NetworkConfig(lab.Default().MustNode("remote1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg, "enp1s0") || strings.Contains(cfg, "10.0.0.15") {
		t.Errorf("unexpected management NIC: %s", cfg)
	}
}

// A machine is given a resolver, and given it on the segment it
// routes by.
//
// It needs one at all because a machine has no images but the ones it
// pulls: k0s runs its node-local balancer as a static pod, so a
// worker that cannot resolve a registry cannot start the balancer,
// cannot reach the API through it, and never registers. The run that
// found this reported "w1, w2 never registered" — four steps from a
// DNS lookup.
//
// And on the lab segment rather than management, because a machine
// resolving over management reaches a service by a path no router in
// the topology explains. That borrowed path is what made an earlier
// run pass, and with it a node could pull images while the segments
// it is supposed to depend on were down.
func TestAMachineIsGivenAResolverOnTheSegmentItRoutesBy(t *testing.T) {
	cfg, err := NetworkConfig(lab.Default().MustNode("w1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, Resolver) {
		t.Fatalf("the machine is given no resolver:\n%s", cfg)
	}

	data := cfg[strings.Index(cfg, GuestInterface(0)+":"):]
	if !strings.Contains(data, Resolver) {
		t.Errorf("the resolver is not on the segment the machine routes by:\n%s", cfg)
	}

}
