package main

import (
	"os"
	"testing"
)

// Exercises the real kernel path: run under a network namespace with a
// device present. Skipped unless asked for, since it writes nftables.
func TestForwardingPathAgainstTheKernel(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside a namespace")
	}
	if err := ensureForwardingPath("dummy0", 1420); err != nil {
		t.Fatalf("ensureForwardingPath: %v", err)
	}
}
