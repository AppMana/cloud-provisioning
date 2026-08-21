package install

import (
	"strings"
	"testing"
)

// Only the kinds, never the controllers.
//
// Cluster API's release is one file carrying both. Applying the whole
// of it would start controllers that reconcile the same machines the
// lab's own infrastructure controller reports, and two things
// reconciling one machine is a race whose failures look like the
// product misbehaving.
func TestOnlyTheKindsSurviveTheFilter(t *testing.T) {
	manifest := `apiVersion: v1
kind: Namespace
metadata:
  name: capi-system
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: clusters.cluster.x-k8s.io
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: capi-controller-manager
spec:
  template:
    spec:
      containers:
        - args: ["--kind=CustomResourceDefinition"]
`
	got := string(OnlyCRDs([]byte(manifest)))

	if !strings.Contains(got, "clusters.cluster.x-k8s.io") {
		t.Error("the CRD was dropped")
	}
	if strings.Contains(got, "capi-controller-manager") {
		t.Error("a controller survived: it would reconcile the same machines the lab's own controller reports")
	}
	if strings.Contains(got, "kind: Namespace") {
		t.Error("a namespace survived the filter")
	}
	// The Deployment mentions the kind in an argument. Matching any
	// line that contains the word would have kept it.
	if strings.Contains(got, "--kind=CustomResourceDefinition") {
		t.Error("a document was kept because it mentioned the kind rather than being one")
	}
}

// A release with no CRDs in it is a failure rather than an empty
// apply, because it means the fetch returned something else and the
// product's reconcilers would then fail to start an informer with no
// explanation.
func TestAReleaseWithNoKindsYieldsNothing(t *testing.T) {
	if got := OnlyCRDs([]byte("apiVersion: v1\nkind: Namespace\n")); got != nil {
		t.Errorf("expected nothing, got %q", got)
	}
}

// The lab's own kinds ship with the harness, so a run cannot
// half-work because a file was somewhere else.
func TestTheLabsOwnKindsAreEmbedded(t *testing.T) {
	body := string(labCRDs)
	for _, kind := range []string{
		"containernetmachines",
		"containernetmachinetemplates",
		"containernetclusters",
	} {
		if !strings.Contains(body, kind) {
			t.Errorf("the embedded CRDs do not define %s", kind)
		}
	}
	if n := strings.Count(body, "kind: CustomResourceDefinition"); n != 3 {
		t.Errorf("%d definitions embedded, want the machine, its template, and the cluster", n)
	}
}

// Cluster API reads a contract label from a provider's CRDs to learn
// which of the provider's API versions satisfies which of its
// contracts. Without it its conversion webhook cannot resolve the
// kind at all: every Cluster stays Provisioning and every Machine
// stays Pending, with the reason only in Cluster API's own log.
//
// A provider that is never run against Cluster API can omit this and
// look entirely correct, which is what happened here for as long as
// the lab ran without the dependency installed.
func TestTheLabsKindsDeclareTheClusterAPIContract(t *testing.T) {
	body := string(labCRDs)
	if n := strings.Count(body, "cluster.x-k8s.io/v1beta2:"); n != 3 {
		t.Errorf("%d kinds declare the v1beta2 contract, want all three", n)
	}
	// The kinds are served at v1beta2, so that is what the label must
	// name: a label pointing at a version the CRD does not serve is
	// the same failure with a different message.
	if !strings.Contains(body, "cluster.x-k8s.io/v1beta2: v1beta2") {
		t.Error("the contract label does not name the version these kinds are served at")
	}
}

// Cluster API”'s manager reads and writes a provider”'s own kinds
// through an aggregated ClusterRole. A provider grants that by
// shipping a role labelled to aggregate into it; without one the
// manager lists nothing, every Cluster and Machine sits in an empty
// phase, and the only account of it is in Cluster API”'s own log.
func TestTheLabsKindsGrantClusterAPIAccessToThem(t *testing.T) {
	body := string(labCRDs)
	if !strings.Contains(body, "cluster.x-k8s.io/aggregate-to-manager: \"true\"") {
		t.Error("no role aggregates into Cluster API'''s manager, so it cannot read these kinds")
	}
	if !strings.Contains(body, "containernet.appmana.com") || !strings.Contains(body, "kind: ClusterRole") {
		t.Error("the aggregated role does not grant this provider'''s group")
	}
}
