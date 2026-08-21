package claim

import "testing"

// A peer entry always carries the machine's tunnel address and its
// node address, both host routes. What says the product resolved the
// machine to a node is something wider: the pod block that node owns.
//
// Waiting on the entry existing at all would pass the moment the
// machine was claimed, long before anything could reach a pod on it.
func TestOnlyAPodBlockCountsAsPublished(t *testing.T) {
	if got := wider("10.100.0.128/32,203.0.113.10/32"); got != "" {
		t.Errorf("host routes were read as pod blocks: %q", got)
	}
	if got := wider("10.100.0.128/32,203.0.113.10/32,10.244.159.0/26"); got != "10.244.159.0/26" {
		t.Errorf("wider = %q, want the pod block alone", got)
	}
	if got := wider(""); got != "" {
		t.Errorf("an empty entry yielded %q", got)
	}
	// IPv6 host routes are host routes too.
	if got := wider("fd00::1/128"); got != "" {
		t.Errorf("an IPv6 host route was read as a pod block: %q", got)
	}
}
