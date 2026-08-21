// Package network installs the cluster's container network, one
// implementation per network.
//
// What varies by network is who owns which prefix and whether the
// tunnel carries pod addresses or node addresses. That difference is
// the reason this is an axis of the matrix at all: a native network
// puts pod addresses on the tunnel and the mesh's accept lists carry
// each node's blocks; an encapsulating one addresses the tunnel's
// packets to nodes, and the controller's own cni package models the
// difference from each network's resources.
package network

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Deps is what an installer needs.
type Deps struct {
	Topology lab.Topology
	Rig      rig.Rig
	Kube     *kube.Client
	Images   cluster.Images
	WorkDir  string
	PodCIDR  string
	// APIServer is an address on the site that every node must be able
	// to reach the cluster by. Networks that autodetect a node's
	// address use it to choose the right one.
	APIServer string
}

// Installer installs one network.
type Installer interface {
	// Name is the network's name, as a row spells it.
	Name() string
	// Install puts it on the cluster and waits for it to be carrying
	// traffic, not merely applied.
	Install(ctx context.Context, d Deps) error
}

// Registry is every network the harness can install.
var Registry = map[string]Installer{}

// Register adds an installer. Called from each implementation's init.
func Register(i Installer) { Registry[i.Name()] = i }

// For returns the installer for a network.
func For(name string) (Installer, error) {
	i, ok := Registry[name]
	if !ok {
		have := make([]string, 0, len(Registry))
		for k := range Registry {
			have = append(have, k)
		}
		return nil, fmt.Errorf("no installer for network %q (have %v)", name, have)
	}
	return i, nil
}

// AllNodes is every node that runs a kubelet, which is every node a
// network has to work on.
func AllNodes(t lab.Topology) []string {
	var out []string
	for _, n := range t.Nodes {
		if n.IsClusterNode() {
			out = append(out, n.Name)
		}
	}
	return out
}
