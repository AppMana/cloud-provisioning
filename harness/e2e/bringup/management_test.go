package bringup

import (
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// Unexpected addresses must remain visible to the isolation proof.
func TestUnexpectedManagementAddressIsNotHidden(t *testing.T) {
	out := []byte("2: enp1s0 inet 10.0.0.15/24 scope global enp1s0\n3: ens2 inet 10.10.0.10/24 scope global ens2\n")
	got, err := (GuestProber{}).parse(out)
	if err != nil || len(got) != 2 {
		t.Fatalf("addresses=%v err=%v", got, err)
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
