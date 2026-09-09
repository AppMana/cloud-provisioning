package vm

import (
	"context"
	"strings"
	"testing"
)

// On the installed OKD 4.21 SCOS site, OVN moved 10.10.0.14/24
// from the sole physical ens2 to br-ex. Cutting ens2 does not transfer
// that address back. A harness restore must leave the CNI's ownership intact.
func TestRestoreDoesNotMoveObservedOVNHostAddressToPhysicalNIC(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)
	if err := n.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range rec.calls {
		command := strings.Join(call, " ")
		if strings.Contains(command, "ip addr") || strings.Contains(command, "ip route") {
			t.Fatalf("restoring the cable changed distribution-owned addressing: %s", command)
		}
	}
	if len(rec.calls) != 1 || !strings.Contains(strings.Join(rec.calls[0], " "), "ip link set ens2 up") {
		t.Fatalf("restore must reconnect the sole physical NIC: %v", rec.calls)
	}
}
