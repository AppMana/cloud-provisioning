// Package cluster builds the site cluster, one implementation per
// distribution.
//
// What varies by distribution is how the site is built, how a remote
// joins, and who balances the API path. The first is here; the second
// is the product's own join provider, which is the thing under test
// and is never reimplemented here; the third is asserted separately.
//
// The site may be built with the distribution's own tooling. That is
// deliberate and is the line this package draws: a lab that
// hand-assembles a cluster proves the harness can assemble one, and
// the remote join — the part that must go through the product — is
// the only part that matters for what is being measured.
package cluster

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Images carries an image from this host onto nodes.
//
// The site has no route to a registry — that is what makes it a site
// — so every image a node needs arrives this way. How it arrives is
// the rig's business: a container has a runtime this host can talk
// to, a machine does not.
type Images interface {
	Load(ctx context.Context, image string, nodes []string, importArgs []string) error
}

// Importer pairs an image carrier with the way one distribution's
// runtime takes an image, so callers that do not know the
// distribution can still carry images in.
type Importer struct {
	Images Images
	Args   []string
}

// Load implements Images.
func (i Importer) Load(ctx context.Context, image string, nodes []string, _ []string) error {
	return i.Images.Load(ctx, image, nodes, i.Args)
}

// Deps is what a builder needs to work.
type Deps struct {
	Topology lab.Topology
	Rig      rig.Rig
	Kube     *kube.Client
	WorkDir  string
	PodCIDR  string
	SvcCIDR  string
	Images   Images
	// Network is what the row installs, or "default" for the one the
	// distribution itself ships. A builder needs it because some
	// distributions must be told at build time whether to bring their
	// own.
	Network string
	// K0sCalicoMTU overrides the bundled Calico overlay MTU for a fresh k0s
	// site. Zero preserves the distribution default.
	K0sCalicoMTU int
	// K0sCalicoManagedAddresses keeps Calico's per-node stored addresses
	// authoritative when the product selects native or WireGuard transport.
	K0sCalicoManagedAddresses bool
}

// Builder builds one distribution's site cluster.
type Builder interface {
	// Name is the distribution's name, as a row spells it.
	Name() string

	// Build brings up the site: control planes, then workers.
	Build(ctx context.Context, d Deps) error

	// KubeletInvariant asserts no kubelet on this site depends on
	// another node's survival.
	//
	// Every distribution answers this differently and every one has to
	// answer it, because a kubelet pinned to one control plane turns
	// that member's death into an outage for a node that had quorum
	// available the whole time. Where the answer lives is
	// distribution-specific, which is why this is on the builder and
	// not in one shared check reading one path.
	KubeletInvariant(ctx context.Context, d Deps) error

	// CRIEndpoint is where crictl finds this distribution's runtime. A
	// crictl aimed at the wrong socket sees no containers, which reads
	// as every path being broken at once.
	CRIEndpoint() string

	// NeedsNodeImage is whether this distribution expects a node that
	// is already a Kubernetes node.
	//
	// Only kubeadm does. The others ship one binary that installs
	// everything below the kubelet, so a machine needs nothing
	// underneath them; kubeadm configures a node and expects the
	// runtime, the plugins and the tools to be there — which for a
	// remote means an image, because a remote is launched and joins
	// on a disk seconds old.
	NeedsNodeImage() bool

	// ImportArgs is how this distribution's runtime takes an image on
	// standard input.
	//
	// A distribution that brings its own containerd does not share the
	// one on the node's PATH, and an image imported into the wrong one
	// is invisible to the kubelet that needs it: the pod sits in
	// ErrImageNeverPull while `ctr images ls` shows the image present,
	// because they are two different runtimes. Measured on k0s, whose
	// containerd held none of the images the default one held.
	ImportArgs() []string
}

// Registry is every distribution the harness can build.
var Registry = map[string]Builder{}

// Register adds a builder. Called from each implementation's init.
func Register(b Builder) { Registry[b.Name()] = b }

// For returns the builder for a distribution.
func For(name string) (Builder, error) {
	b, ok := Registry[name]
	if !ok {
		have := make([]string, 0, len(Registry))
		for k := range Registry {
			have = append(have, k)
		}
		return nil, fmt.Errorf("no site builder for %q (have %v)", name, have)
	}
	return b, nil
}

// SiteNodes are the cluster nodes at the site, in build order:
// control planes first, because a worker has nothing to join until
// one exists.
func SiteNodes(t lab.Topology) []lab.Node {
	return append(t.NodesInRole(lab.ControlPlane), t.NodesInRole(lab.Worker)...)
}

// ControlPlaneAddresses are the site addresses of the members, which
// is what every node balances across and what the bastion dials.
func ControlPlaneAddresses(t lab.Topology) []string {
	var out []string
	for _, n := range t.NodesInRole(lab.ControlPlane) {
		out = append(out, n.Address(lab.LANSegment))
	}
	return out
}

// SiteReuser verifies a preserved installation and restores its client and
// native join credentials. A runner must not infer this capability from a name.
type SiteReuser interface {
	Reuse(context.Context, Deps) error
}

// WorkerVerifier observes the distribution's pinned Linux runtime on a remote
// machine. It does not install, repair, join, or choose a machine transport.
// AWS and local VM rigs provide the same Node interface to this check.
type WorkerVerifier interface {
	VerifyWorker(context.Context, rig.Node) error
}
