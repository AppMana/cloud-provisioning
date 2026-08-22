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

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"

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

	// Encapsulation is what this network does to a pod packet, in the
	// product's own terms.
	//
	// It decides what the mesh should be carrying for a remote, and
	// the two answers are opposites: a native network puts pod
	// addresses on the tunnel, so each peer's accept list carries the
	// blocks its node owns; an encapsulating one addresses its packets
	// to nodes, so the list carries node addresses and no blocks at
	// all. A harness that expected blocks either way would fail every
	// encapsulating row for doing the right thing — and did, the first
	// time there were two networks to be wrong about.
	Encapsulation() cni.Encapsulation
	// Install puts it on the cluster and waits for it to be carrying
	// traffic, not merely applied.
	Install(ctx context.Context, d Deps) error
	// LoadImages carries whatever this network needs onto nodes that
	// have only just acquired a runtime.
	//
	// Separate from Install because a remote cannot receive any of it
	// at install time: the network goes on before any remote has
	// joined, and a remote has no container runtime of its own until
	// it does. Its images have to arrive after the join, into the
	// runtime its kubelet actually talks to — which on a distribution
	// that brings its own containerd is not the one on the node's
	// PATH.
	//
	// Images are not the whole of it, which the name understates. A
	// network that chains a delegated plugin needs that plugin here
	// too: the site's nodes were given it at install time and a remote
	// was not, so the remote creates no pod sandbox at all and its
	// probe sits in ContainerCreating while every agent on it reports
	// running.
	LoadImages(ctx context.Context, d Deps, nodes []string) error
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

// SiteNodes is every cluster node at the site: the ones that exist
// when a network is installed.
func SiteNodes(t lab.Topology) []string {
	var out []string
	for _, n := range t.Nodes {
		if n.Role == lab.ControlPlane || n.Role == lab.Worker {
			out = append(out, n.Name)
		}
	}
	return out
}
