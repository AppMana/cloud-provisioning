package install

import (
	"strings"
	"testing"
)

// The lab's own kinds ship with the harness, so a run cannot
// half-work because a file was somewhere else.
func TestTheLabsOwnKindsAreEmbedded(t *testing.T) {
	body := string(labCRDs)
	for _, kind := range []string{
		"containernetmachines",
		"containernetmachinetemplates",
		"containernetclusters",
		"importedcontrolplanes",
	} {
		if !strings.Contains(body, kind) {
			t.Errorf("the embedded CRDs do not define %s", kind)
		}
	}
	if n := strings.Count(body, "kind: CustomResourceDefinition"); n != 4 {
		t.Errorf("%d definitions embedded, want machine, template, infrastructure cluster and imported control plane", n)
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
	if n := strings.Count(body, "cluster.x-k8s.io/v1beta2:"); n != 4 {
		t.Errorf("%d kinds declare the v1beta2 contract, want all four", n)
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
