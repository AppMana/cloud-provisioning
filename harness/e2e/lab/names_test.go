package lab

import "testing"

// Every name this topology gives an interface has to be one the
// kernel will take.
//
// It will not take more than fifteen characters, and it does not say
// so when the name is chosen: the creation fails quietly and the next
// command reports "Cannot find device", which reads as something
// having gone missing rather than never having been made.
// "cldt-mgmt-remote1" is seventeen.
func TestEveryInterfaceNameFitsTheKernelsLimit(t *testing.T) {
	topo := Default()

	for _, l := range topo.Links() {
		_, endpoint, ok := cutLast(l.To, ":")
		if !ok {
			t.Fatalf("link %q has no endpoint name", l.To)
		}
		if len(endpoint) > MaxInterfaceName {
			t.Errorf("link endpoint %q is %d characters, and the kernel takes %d",
				endpoint, len(endpoint), MaxInterfaceName)
		}
	}
	for _, s := range topo.Segments {
		if len(s) > MaxInterfaceName {
			t.Errorf("segment %q is %d characters, and the kernel takes %d",
				s, len(s), MaxInterfaceName)
		}
	}
}

func cutLast(s, sep string) (string, string, bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if string(s[i]) == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
