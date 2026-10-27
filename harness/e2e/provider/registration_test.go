package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// The product's join pattern starts the kubelet with --provider-id, so a
// registered node already names its machine. The harness only observes
// that: it never writes a Node, and a node registered without the
// machine's identity is the product's failure, reported at once.
func TestRegistrationObservesTheProductsProviderID(t *testing.T) {
	topo := lab.Default()
	remote := topo.MustNode("remote1")
	address := remote.Address(remote.Interfaces[0].Segment)
	machine := `{"metadata":{"uid":"u"},"spec":{"nodeName":"remote1"},"status":{"ready":true}}`
	for _, tc := range []struct {
		registered string
		ok         bool
	}{
		{ProviderID("cldt", "remote1"), true},
		{"", false},
		{"labcontainers://cldt/remote2", false},
	} {
		k := &fakeKube{objects: map[string]string{"remote1": machine}, nodes: fmt.Sprintf("remote1 %s,\n", address), nodeProviderID: tc.registered}
		c := &Controller{Kube: k.client(), Topology: topo, LabName: "cldt"}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := c.WaitRegistration(ctx, "cloud-provisioning", time.Millisecond)
		cancel()
		if (err == nil) != tc.ok {
			t.Errorf("node registered with %q: %v", tc.registered, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "providerID")) {
			t.Errorf("failure does not name the providerID: %v", err)
		}
		for _, call := range k.calls {
			if contains(call, "node") && contains(call, "patch") {
				t.Fatalf("the harness wrote a Node: %v", call)
			}
			if strings.Contains(strings.Join(call, " "), "nodeRef") {
				t.Fatalf("the harness wrote nodeRef, which is Cluster API's job: %v", call)
			}
		}
	}
}
