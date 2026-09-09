package coreos

import (
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Reusing the VM transport must not grant Ignition guests Ubuntu's cloud-init
// observer. Native completion needs an Ignition-specific implementation.
func TestIgnitionGuestDoesNotInheritCloudInitObservers(t *testing.T) {
	r := New(lab.Default(), t.TempDir(), "unused.iso", "unused.qcow2")
	n := r.Node("remote1")
	if _, ok := n.(rig.BootstrapConsumer); !ok {
		t.Fatal("Ignition bootstrap capability missing")
	}
	if _, ok := n.(rig.BootstrapHealth); ok {
		t.Fatal("Ignition guest acquired Linux cloud-init failure observer")
	}
	if _, ok := n.(rig.BootstrapCompletion); ok {
		t.Fatal("Ignition guest acquired Linux cloud-init completion observer")
	}
}
