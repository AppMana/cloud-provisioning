package vm

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// A guest whose agent answers is not yet a machine with its NIC. Under
// the SDK launcher the guest agent was measured answering while the
// declared Ethernet device did not exist yet, and addressing it then
// failed with "Cannot find device ens2" a few seconds before the device
// appeared. Ready means every declared interface is there.
func TestAMachineIsReadyOnlyWithItsDeclaredInterfaces(t *testing.T) {
	asked := 0
	missing := 2
	run := func(_ context.Context, _ io.Reader, argv ...string) ([]byte, []byte, int, error) {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "ip link show dev "+GuestInterface(0)) {
			asked++
			if asked <= missing {
				return nil, []byte(`Device "` + GuestInterface(0) + `" does not exist.`), 1, nil
			}
		}
		return nil, nil, 0, nil
	}
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: run}
	if err := r.waitForNode(context.Background(), "remote1", time.Minute); err != nil {
		t.Fatalf("waitForNode: %v", err)
	}
	if asked != missing+1 {
		t.Errorf("the interface was checked %d times, want readiness to wait until it existed (%d)", asked, missing+1)
	}
}
