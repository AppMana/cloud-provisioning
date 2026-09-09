package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type failedBootstrapNode struct {
	rig.Node
	failure error
	probes  int
}

func (n *failedBootstrapNode) BootstrapFailure(context.Context) error { n.probes++; return n.failure }

type bootstrapHealthRig struct {
	rig.Rig
	node rig.Node
}

func (r bootstrapHealthRig) Node(string) rig.Node { return r.node }

func TestRegistrationStopsOnObservedTerminalBootstrapFailure(t *testing.T) {
	failure := errors.New("verified bootstrap failure")
	node := &failedBootstrapNode{failure: failure}
	k := &fakeKube{objects: map[string]string{"remote1": `{"metadata":{"uid":"instance-uid","annotations":{"lab.cloud-provisioning.appmana.com/bootstrapped-uid":"instance-uid"},"finalizers":["lab.cloud-provisioning.appmana.com/instance"]},"spec":{"containerName":"clab-cldt-remote1"},"status":{"ready":true}}`}, nodes: "cp 10.10.0.10,\n"}
	c := &Controller{Kube: k.client(), Rig: bootstrapHealthRig{node: node}, Topology: lab.Default(), LabName: "cldt"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.AdoptNodes(ctx, "cloud-provisioning", time.Millisecond); !errors.Is(err, failure) {
		t.Fatalf("terminal failure hidden behind registration wait: %v", err)
	}
	if node.probes != 1 {
		t.Fatalf("unexpected repeated probes: %d", node.probes)
	}
}
