// Package rig is what a node is made of.
//
// Every stage of the harness reaches nodes through this interface and
// never through docker or ssh directly, so that a row runs unchanged
// on either rig. That is the whole point: a container row and a VM
// row must differ in what a node *is* and in nothing else, or the two
// are not comparable and a divergence between them says nothing.
//
// The seam it replaces was three shell helpers copied into nine
// files:
//
//	c()       { echo "clab-$LAB-$1"; }
//	in_node() { docker exec "$(c "$1")" "${@:2}"; }
//	k()       { in_node bastion kubectl "$@"; }
package rig

import labrig "github.com/appmana/labcontainers/pkg/rig"

// Node is one machine in the lab.
type Node = labrig.Node

// Nodes resolves machines for measurement without granting provisioning or
// teardown operations. A fleet may combine local VMs and CAPA-owned instances.
type Nodes = labrig.Nodes

// Rig builds and tears down a lab.
type Rig = labrig.Rig

// Fleet overlays explicitly bound remote machines on an existing site. It
// deliberately implements Nodes, not Rig: CAPA owns the remote lifecycle.
type Fleet = labrig.Fleet

// ExitError is a command that ran and failed. Stderr is carried
// because a harness that reports only an exit status makes every
// failure a second investigation.
type ExitError = labrig.ExitError
