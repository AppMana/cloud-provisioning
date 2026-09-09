package claim

import (
	"strings"
	"testing"
)

// The claim names a template and nothing else. It must not carry an
// address, a token, or a peer: everything about where the machine is
// and how it joins comes from the product, and a claim that supplied
// any of it would make a row prove the harness can join a node rather
// than that the product can.
func TestTheClaimSuppliesNothingTheProductShouldDerive(t *testing.T) {
	objects := Objects("remote1", "clab-cldt-remote1")

	for _, forbidden := range []string{
		"203.0.113",  // an address
		"192.0.2",    // another
		"joinToken",  // a credential
		"peer",       // a peer list
		"wg-dialer",  // anything about the tunnel
		"providerID", // what the infrastructure controller reports
		"addresses",  // likewise
	} {
		if strings.Contains(objects, forbidden) {
			t.Errorf("the claim carries %q, which the product is supposed to derive", forbidden)
		}
	}

	// What it must carry: the binding, in the field the API defines
	// for it.
	if !strings.Contains(objects, "containerName: clab-cldt-remote1") {
		t.Error("the template does not say which machine backs it")
	}
	if !strings.Contains(objects, "kind: ProvisionedNodeClaim") {
		t.Error("no claim: this is the one resource an operator commits per node")
	}
	// And the claim points at the template rather than at a machine:
	// the reconciler creates the machine from it, which is the path
	// under test.
	if !strings.Contains(objects, "kind: ContainernetMachineTemplate") {
		t.Error("the claim does not name a template")
	}
}

// Every object is namespaced and named consistently, or the claim
// reconciler creates a machine somewhere the rest of the harness does
// not look.
func TestEveryObjectLandsInTheSameNamespace(t *testing.T) {
	objects := Objects("remote1", "clab-cldt-remote1")
	docs := strings.Split(objects, "\n---\n")
	if len(docs) != 4 {
		t.Fatalf("%d objects, want the cluster, its infrastructure, the template and the claim", len(docs))
	}
	for _, doc := range docs {
		if !strings.Contains(doc, "namespace: "+Namespace) {
			t.Errorf("an object is not in %s:\n%s", Namespace, doc)
		}
	}
}
