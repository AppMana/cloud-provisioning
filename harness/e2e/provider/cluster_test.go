package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// Cluster API will not move a Cluster out of Provisioning until its
// infrastructure reports ready, and until then every Machine in it
// stays Pending — so nothing is linked to a node and the mesh is
// never told a peer exists. Reporting the cluster is the provider's
// job, the same way CAPA reports an AWSCluster.
func TestTheInfrastructureClusterIsReported(t *testing.T) {
	k := &fakeKube{}
	c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt"}

	if err := c.ReconcileCluster(context.Background(), "cloud-provisioning", "cldt",
		lab.SitePrefix+".10", 6443); err != nil {
		t.Fatal(err)
	}

	endpoint, ready := -1, -1
	for i, call := range k.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "controlPlaneEndpoint") {
			endpoint = i
		}
		if strings.Contains(joined, `"ready":true`) {
			ready = i
		}
	}
	if endpoint < 0 {
		t.Fatalf("no endpoint was stated: %v", k.calls)
	}
	if ready < 0 {
		t.Fatalf("the cluster was never reported ready: %v", k.calls)
	}
	// Spec before status, as everywhere else: an endpoint has to be
	// readable before anything is told the cluster is usable.
	if endpoint > ready {
		t.Error("the cluster was reported ready before it had an endpoint")
	}
}

// The retained MicroK8s campaign was Ready with an obsolete 6443 endpoint.
// Readiness must not prevent correcting the port to the observed 16443 API.
func TestReadyInfrastructureEndpointCanBeCorrected(t *testing.T) {
	k := &fakeKube{clusterReady: "true"}
	c := &Controller{Kube: k.client()}
	if err := c.ReconcileCluster(context.Background(), "cloud-provisioning", "cldt", "10.10.0.10", 16443); err != nil {
		t.Fatal(err)
	}
	patched := false
	for _, call := range k.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, `"controlPlaneEndpoint":{"host":"10.10.0.10","port":16443}`) {
			patched = true
		}
		if strings.Contains(joined, "--subresource=status") {
			t.Fatal("already-ready cluster status was rewritten")
		}
	}
	if !patched {
		t.Fatal("Ready status hid the obsolete endpoint")
	}
}
