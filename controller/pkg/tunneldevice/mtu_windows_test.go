package tunneldevice

import (
	"net/netip"
	"os"
	"strconv"
	"testing"
)

// Read the selected native egress without creating or changing any interface.
func TestNativeTransportMTUReadOnly(t *testing.T) {
	destination := os.Getenv("CLDT_TRANSPORT_TEST_DESTINATION")
	if destination == "" {
		t.Skip("requires an explicit native transport destination and expected MTU")
	}
	expected, err := strconv.ParseUint(os.Getenv("CLDT_TRANSPORT_TEST_MTU"), 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	mtu, err := transportPathMTU(netip.MustParseAddr(destination), 0)
	if err != nil || mtu != uint32(expected) {
		t.Fatalf("native egress MTU = %d, %v; want %d", mtu, err, expected)
	}
	budget, err := transportMTU([]uint32{mtu})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native egress MTU %d, tunnel budget %d", mtu, budget)
}
