package main

import (
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

func TestRecoveryOrderPlumbsAppliancesBeforeMachines(t *testing.T) {
	topo, err := lab.WithRemoteSlots(3)
	if err != nil {
		t.Fatal(err)
	}
	order := recoveryOrder(topo)
	if len(order) != len(topo.Nodes) {
		t.Fatalf("recovery covers %d of %d nodes", len(order), len(topo.Nodes))
	}
	seen := map[string]bool{}
	machines := false
	for _, n := range order {
		if seen[n.Name] {
			t.Fatalf("%s recovered twice", n.Name)
		}
		seen[n.Name] = true
		if n.IsClusterNode() {
			machines = true
			continue
		}
		if machines {
			t.Fatalf("appliance %s ordered after a machine; machines route through the appliances", n.Name)
		}
	}
	for _, n := range topo.Nodes {
		if !seen[n.Name] {
			t.Fatalf("%s never recovered", n.Name)
		}
	}
}

func TestRigNodesKeepsOnlyTopologyNodes(t *testing.T) {
	topo := lab.Default()
	registered := []string{"aws-win2022-gpu", "cp", "ip-172-29-0-9", "remote1", "w1"}
	got := rigNodes(topo, registered)
	want := []string{"cp", "remote1", "w1"}
	if len(got) != len(want) {
		t.Fatalf("rig nodes %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rig nodes %v, want %v", got, want)
		}
	}
}
