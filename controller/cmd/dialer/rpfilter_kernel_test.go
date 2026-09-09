package main

import (
	"os"
	"strings"
	"testing"
)

// A peer whose own address this node routes through the tunnel still
// sends its WireGuard packets over the underlay, and strict reverse
// path filtering drops exactly those: the route back to that source
// does not leave by the interface they arrived on. The kernel takes
// max(conf/all, conf/<iface>), so relaxing "all" alone cannot save an
// interface that is itself strict, which is what a NIC restored after
// a reboot inherits from conf/default.
//
// Measured before this existed: a rebooted remote with all=0 and a
// restored eth1 at 1, its peer's handshake responses on the wire,
// IPReversePathFilter counting every one, WireGuard's rx frozen, and
// that node's cloud-to-cloud tunnel dead while every other path
// worked.
//
// Exercises the real kernel: run under a network namespace.
func TestEveryStrictReversePathKnobIsRelaxed(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside a namespace")
	}

	// The shape that broke: all off, the rest strict.
	for knob, value := range map[string]string{
		"ipv4/conf/all/rp_filter":     "0",
		"ipv4/conf/default/rp_filter": "1",
		"ipv4/conf/lo/rp_filter":      "1",
	} {
		if err := setSysctl(knob, value); err != nil {
			t.Fatalf("staging %s: %v", knob, err)
		}
	}

	knobs := reversePathKnobs("cldttest0")
	found := false
	for _, k := range knobs {
		if strings.Contains(k, "/lo/") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the enumeration missed an interface this node has: %v", knobs)
	}

	// The production path, not a copy of it: a test that reimplements
	// the loop passes against the very logic that shipped the bug.
	relaxReversePathFiltering("cldttest0")

	// What the kernel actually enforces on an interface is max(all,
	// iface): strict anywhere in that pair drops the peer's handshake
	// before any socket sees it.
	all, err := readSysctl("ipv4/conf/all/rp_filter")
	if err != nil {
		t.Fatal(err)
	}
	lo, err := readSysctl("ipv4/conf/lo/rp_filter")
	if err != nil {
		t.Fatal(err)
	}
	if all == "1" || lo == "1" {
		t.Errorf("reverse path filtering is still strict (all=%s lo=%s): a peer routed through the tunnel cannot complete a handshake", all, lo)
	}
	// What was off stays off: this relaxes, it never adds a check the
	// node was not already making.
	if all != "0" {
		t.Errorf("conf/all was 0 and became %s: this must never tighten", all)
	}
	// And the template new interfaces inherit must not hand the next
	// NIC the same strictness, which is how a reboot reintroduced it.
	def, err := readSysctl("ipv4/conf/default/rp_filter")
	if err != nil {
		t.Fatal(err)
	}
	if def == "1" {
		t.Error("conf/default is still strict, so the next interface the platform creates starts strict again")
	}
}
