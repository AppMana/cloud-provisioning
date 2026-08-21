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
