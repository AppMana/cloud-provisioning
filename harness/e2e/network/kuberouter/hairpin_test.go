package kuberouter

import (
	"strings"
	"testing"
)

// A pod that dials its own service is DNATed straight back to itself,
// and without hairpin on its bridge port that frame has nowhere to
// go. Unset, the row fails later as a handful of service checks,
// which reads as the network being broken rather than as one field
// being absent.
func TestTheBridgeReflectsAFrameBackOutItsOwnPort(t *testing.T) {
	conf := `      "bridge":{"isDefaultGateway":true,"mtu":1500}`
	out, err := hairpin([]byte(conf))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"hairpinMode":true`) {
		t.Errorf("hairpin was not set:\n%s", out)
	}
}

// And a manifest that has changed shape fails here rather than
// passing through unchanged: a substitution that matched nothing
// leaves the row proving the opposite of what it says.
func TestAManifestThatCannotBeHairpinnedIsAFailure(t *testing.T) {
	if _, err := hairpin([]byte(`      "bridge":{"mtu":1500}`)); err == nil {
		t.Fatal("a manifest with no anchor was accepted, so hairpin was silently not set")
	}
}
