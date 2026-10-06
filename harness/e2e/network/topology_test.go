package network

import (
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// A profile decides the site it is built on, and every tool that
// reaches an existing site has to agree with the one that built it: the
// AWS row reaching the dual-stack identity site through the default
// topology would ask a control plane at an address no node holds.
func TestAProfileNamesItsSiteTopology(t *testing.T) {
	identity, err := Select("k0s", "calico-bird-dualstack")
	if err != nil {
		t.Fatal(err)
	}
	got, err := identity.SiteTopology(2)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := lab.WithIdentities(2)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("calico-bird-dualstack site is not the identity topology")
	}
	plain, err := Select("k0s", "calico")
	if err != nil {
		t.Fatal(err)
	}
	got, err = plain.SiteTopology(2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, lab.Default()) {
		t.Errorf("k0s/calico site is not the default topology")
	}
}
