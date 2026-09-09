package main

import (
	"fmt"
	"github.com/appmana/cloud-provisioning/harness/e2e/install"
	"strings"
)

// preconditions refuses a set of flags the run cannot honour.
//
// The stages nest: a network needs a cluster, the product needs a
// network, a remote needs the product, the matrix needs somewhere to
// run, and an outage row is a matrix taken three times. Asking for an
// inner stage without an outer one used to do nothing at all — no
// warning, no failure, and a run that ended by printing every
// assertion it had passed.
//
// That is how a kubeadm row on machines reported a site built, a
// network installed and every node Ready, having quietly not run the
// reachability matrix it was asked for. A flag that does nothing is
// worse than one that fails: the run looks like the thing it was
// supposed to prove.
func preconditions(distro, cni string, product bool, remotes, outages string, checks bool) error {
	switch {
	case cni != "" && distro == "":
		return fmt.Errorf("-cni %s needs a cluster to install into: pass -distro", cni)
	case product && cni == "":
		return fmt.Errorf("-product needs a container network: pass -cni")
	case remotes != "" && !product:
		return fmt.Errorf("-remotes %s needs the product that renders their bootstrap: pass -product", remotes)
	case checks && !product:
		return fmt.Errorf("-check needs the product installed, because the matrix measures the paths it creates: pass -product")
	case outages != "" && !checks:
		return fmt.Errorf("-outage %s is the matrix taken before, during and after: pass -check", outages)
	}
	return nil
}

// Validate every requested placement before provisioning. A nonempty flag that
// parses to no placements must not silently skip lifecycle and outage checks.
func validatePlacements(raw string, checks bool) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if !checks {
		return fmt.Errorf("-placements requires -check")
	}
	count := 0
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, err := install.PlacementNamed(name); err != nil {
			return fmt.Errorf("-placements: %w", err)
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("-placements must contain at least one placement")
	}
	return nil
}
