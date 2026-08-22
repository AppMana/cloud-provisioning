package main

import "fmt"

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
