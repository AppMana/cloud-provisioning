package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// The harness states the existing site's endpoint, as an operator would, and
// never reports the cluster ready: the LabCluster is managed-by external, so
// that is the product's job, and writing it here would hide whether it works.
func TestTheHarnessStatesTheEndpointAndLeavesReadinessToTheProduct(t *testing.T) {
	k := &fakeKube{}
	c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt"}
	if err := c.ReconcileCluster(context.Background(), "cloud-provisioning", "cldt", lab.SitePrefix+".10", 16443); err != nil {
		t.Fatal(err)
	}
	patched := false
	for _, call := range k.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, `"controlPlaneEndpoint":{"host":"`+lab.SitePrefix+`.10","port":16443}`) {
			patched = true
		}
		if strings.Contains(joined, "--subresource=status") {
			t.Fatalf("the harness wrote cluster status: %v", call)
		}
	}
	if !patched {
		t.Fatalf("no endpoint was stated: %v", k.calls)
	}
}
