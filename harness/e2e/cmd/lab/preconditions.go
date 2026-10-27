package main

import (
	"fmt"
	"github.com/appmana/cloud-provisioning/harness/e2e/install"
	"strings"
)

// preconditions rejects flags whose required stages are disabled. Network,
// product, remote, matrix, and outage stages depend on their preceding
// stages; a requested check must not be silently skipped.
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
func validatePlacements(raw, distro string, checks bool) error {
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
		if _, err := install.PlacementNamed(name, distro); err != nil {
			return fmt.Errorf("-placements: %w", err)
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("-placements must contain at least one placement")
	}
	return nil
}
