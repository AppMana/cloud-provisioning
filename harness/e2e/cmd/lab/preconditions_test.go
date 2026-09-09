package main

import (
	"strings"
	"testing"
)

// A flag the run cannot honour has to fail. Silently skipped, the run
// ends by printing every assertion it did pass and looks exactly like
// one that proved what was asked.
func TestAFlagTheRunCannotHonourIsRefused(t *testing.T) {
	for _, c := range []struct {
		name    string
		distro  string
		cni     string
		product bool
		remotes string
		outages string
		checks  bool
		want    string
	}{
		{name: "the matrix without the product", distro: "kubeadm", cni: "calico", checks: true, want: "-check"},
		{name: "the product without a network", distro: "k0s", product: true, want: "-product"},
		{name: "a network without a cluster", cni: "calico", want: "-cni"},
		{name: "remotes without the product", distro: "k0s", cni: "calico", remotes: "remote1", want: "-remotes"},
		{name: "an outage without the matrix", distro: "k0s", cni: "calico", product: true, outages: "remote1:reboot", want: "-outage"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := preconditions(c.distro, c.cni, c.product, c.remotes, c.outages, c.checks)
			if err == nil {
				t.Fatal("accepted, so the run will report success having skipped what was asked for")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the failure does not name the flag that was ignored: %v", err)
			}
		})
	}
}

// And a whole row is accepted, as is a bare lab.
func TestACompleteRowIsAccepted(t *testing.T) {
	if err := preconditions("k0s", "calico", true, "remote1,remote2", "remote1:reboot", true); err != nil {
		t.Errorf("a complete row was refused: %v", err)
	}
	if err := preconditions("", "", false, "", "", false); err != nil {
		t.Errorf("a bare lab was refused: %v", err)
	}
}

func TestPlacementRequestsCannotSilentlySkipChecks(t *testing.T) {
	for _, c := range []struct {
		raw           string
		checks, valid bool
	}{
		{"", false, true}, {"  ", true, true}, {", ,", true, false},
		{"one-worker", false, false}, {"one-worker,typo", true, false},
		{" control-plane,one-worker,two-workers,all-nodes ", true, true},
		{",two-workers,", true, true},
	} {
		if err := validatePlacements(c.raw, c.checks); (err == nil) != c.valid {
			t.Errorf("placements=%q check=%t: %v", c.raw, c.checks, err)
		}
	}
}
