package bringup

import (
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// A machine's management address is not an address the lab gave
// anyone, and counting it makes the isolation proof answer a question
// it was not asked.
//
// qemu's usermode network hands every guest the same one, so every
// machine holds 10.0.0.15. The proof walks every address a site node
// holds and asks whether a remote can reach it; with that address in
// the list the answer is yes, for the remote's own interface, which
// is the one thing that is not a path between them. Measured on the
// first VM bring-up: "remote1 reached cp at 10.0.0.15".
func TestAMachinesManagementAddressIsNotPartOfTheLab(t *testing.T) {
	out := []byte("" +
		"2: enp1s0    inet 10.0.0.15/24 scope global enp1s0\\       valid_lft forever\n" +
		"3: ens2    inet 10.10.0.10/24 scope global ens2\\       valid_lft forever\n")

	got, err := GuestProber{Rig: nil}.parse(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if strings.HasPrefix(a, ManagementPrefix) {
			t.Errorf("the management address %s was counted as one the lab gave", a)
		}
	}
	if len(got) != 1 || got[0] != "10.10.0.10" {
		t.Errorf("addresses = %v, want the segment address alone", got)
	}
}

// And a container's addresses are all the lab's, because a container
// has no management link in this topology at all.
func TestAContainerHasNoManagementAddressToExclude(t *testing.T) {
	if !strings.HasPrefix(lab.SitePrefix, "10.10.") {
		t.Skip("the site prefix moved; this test's premise needs revisiting")
	}
	out := []byte("2: eth1    inet 10.10.0.2/24 scope global eth1\\       valid_lft forever\n")
	got := parseAddresses(out)
	if len(got) != 1 || got[0] != "10.10.0.2" {
		t.Errorf("addresses = %v", got)
	}
}
