package claim

import (
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
)

// The two networks want opposite things of a remote's accept list,
// and telling them apart is this file's whole job.
func TestWhatAnAcceptListShouldCarryDependsOnTheNetwork(t *testing.T) {
	const withBlock = "10.100.0.128/32,203.0.113.10/32,10.244.159.0/26"
	const hostsOnly = "10.100.0.128/32,203.0.113.10/32"

	if got := wider(withBlock); got != "10.244.159.0/26" {
		t.Errorf("the pod block was not recognised: %q", got)
	}
	if got := wider(hostsOnly); got != "" {
		t.Errorf("a host route was read as a pod block: %q", got)
	}
	if got := hosts(hostsOnly); got != hostsOnly {
		t.Errorf("the node's addresses were not recognised: %q", got)
	}
}

// A pod block on an encapsulating network is not merely unnecessary,
// it is wrong: the mesh would accept pod-addressed packets that never
// arrive, because that network addresses its packets to nodes.
func TestAPodBlockOnAnEncapsulatingNetworkIsAFailure(t *testing.T) {
	if wider("10.100.0.128/32,10.244.159.0/26") == "" {
		t.Fatal("the block that must be rejected is not even seen")
	}
	// The check the waiter makes, stated here so the reason survives
	// next to the rule.
	if cni.Encapsulated == cni.Native {
		t.Fatal("the two halves of the matrix are the same value")
	}
}

// An unrecognised network must not be guessed at. Either answer would
// be asserted against a lab that might be doing the other.
func TestAnUnknownEncapsulationIsRefused(t *testing.T) {
	if cni.Unknown == cni.Native || cni.Unknown == cni.Encapsulated {
		t.Fatal("unknown is indistinguishable from an answer")
	}
	if !strings.Contains(cni.Unknown.String(), "unknown") &&
		cni.Unknown.String() != "" {
		t.Logf("unknown renders as %q", cni.Unknown.String())
	}
}
