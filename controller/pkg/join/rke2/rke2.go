// Package rke2 implements join.ClusterJoinProvider for RKE2.
//
// Confirmed by reading RKE2's own source: RKE2 is k3s's server code
// under another name for everything this provider touches. `rke2
// token` wires k3s's token.Create/Delete/Generate/List/Rotate verbatim
// (rke2 pkg/cli/cmds/token.go), the supervisor is k3s's same router
// serving agent joins on port 9345 instead of the API port (rke2
// pkg/cli/defaults/defaults.go: cmds.ServerConfig.SupervisorPort =
// 9345), and version.Program only changes the default bootstrap group
// and the kubelet's version suffix. So the provider is k3s's provider
// with RKE2's constants (see k3s.Flavor), and the token format,
// CA-hash algorithm, and bootstrap-Secret shape are documented and
// tested once, in pkg/join/k3s.
package rke2

import (
	"context"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/appmana/cloud-provisioning/controller/pkg/join/k3s"
)

// Provider implements join.ClusterJoinProvider for RKE2.
type Provider struct {
	Client kubernetes.Interface
	// APIAddress is this cluster's own API server address as reached
	// from a newly-joining node (e.g. "https://10.101.0.1:6443"). The
	// agent registers through the same host's supervisor port, which
	// the flavor supplies.
	APIAddress string
	// TTL is how long the minted token remains valid; RKE2's own
	// tokencleaner reaps the Secret at expiry.
	TTL time.Duration
}

var rke2Flavor = k3s.Flavor{
	VersionSuffix:  "+rke2",
	VersionKey:     "rke2Version",
	ExtraGroup:     "system:bootstrappers:rke2:default-node-token",
	SupervisorPort: "9345",
}

// JoinValues implements join.ClusterJoinProvider.
func (p *Provider) JoinValues(ctx context.Context) (map[string]any, error) {
	inner := &k3s.Provider{Client: p.Client, APIAddress: p.APIAddress, TTL: p.TTL, Flavor: rke2Flavor}
	return inner.JoinValues(ctx)
}
