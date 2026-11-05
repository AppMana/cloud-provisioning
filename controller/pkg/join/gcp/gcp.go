// Package gcp implements join.InfraProvider and join.MachineProvisioner
// for CAPG (cluster-api-provider-gcp), enabling Google Cloud Compute Engine
// worker machines alongside AWS, Docker, and Labcontainers in the same cluster.
package gcp

import (
	"context"
	"encoding/base64"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Provider supplies CAPG identity and bootstrap template values. Machine shape
// is copied from a GCPMachineTemplate by the claim reconciler.
type Provider struct{}

var (
	gvk           = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "GCPMachine"}
	gcpClusterGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "GCPCluster"}
)

// GVK implements join.InfraProvider.
func (Provider) GVK() schema.GroupVersionKind { return gvk }

// ClusterGVK implements join.MachineProvisioner.
func (Provider) ClusterGVK() schema.GroupVersionKind { return gcpClusterGVK }

// BootstrapIdentityScript discovers the immutable GCE instance identity after launch.
// CAPG binds a Machine to a Node via gce://<project-id>/<zone>/<instance-name>.
const BootstrapIdentityScript = `#!/bin/sh
set -eu
base="http://metadata.google.internal/computeMetadata/v1"
hdr="Metadata-Flavor: Google"
project=$(curl -fsS --connect-timeout 2 --max-time 5 -H "$hdr" "$base/project/project-id")
zone_path=$(curl -fsS --connect-timeout 2 --max-time 5 -H "$hdr" "$base/instance/zone")
zone=${zone_path##*/}
name=$(curl -fsS --connect-timeout 2 --max-time 5 -H "$hdr" "$base/instance/name")
case "$project$zone$name" in *[!a-z0-9-]*|'') exit 1 ;; esac
[ -n "$project" ] && [ -n "$zone" ] && [ -n "$name" ]
printf 'gce://%s/%s/%s\n' "$project" "$zone" "$name"
`

// BootstrapIdentityPowerShell is the native Windows implementation of GCE
// metadata discovery for k0s Windows workers on Google Cloud.
const BootstrapIdentityPowerShell = `$ErrorActionPreference='Stop'
$base='http://metadata.google.internal/computeMetadata/v1'
$headers=@{'Metadata-Flavor'='Google'}
$project=(Invoke-RestMethod -Uri "$base/project/project-id" -Headers $headers -TimeoutSec 5).Trim()
$zonePath=(Invoke-RestMethod -Uri "$base/instance/zone" -Headers $headers -TimeoutSec 5).Trim()
$zone=($zonePath -split '/')[-1]
$name=(Invoke-RestMethod -Uri "$base/instance/name" -Headers $headers -TimeoutSec 5).Trim()
$ip=(Invoke-RestMethod -Uri "$base/instance/network-interfaces/0/ip" -Headers $headers -TimeoutSec 5).Trim()
if ($project -notmatch '^[a-z0-9-]+$' -or $zone -notmatch '^[a-z0-9-]+$' -or $name -notmatch '^[a-z0-9-]+$') { throw 'Invalid GCE identity' }
$parsed=[Net.IPAddress]::Parse($ip)
if ($parsed.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork) { throw 'Expected primary IPv4 address' }
@{providerID="gce://$project/$zone/$name";nodeAddress=$parsed.ToString()}
`

// InfraValues implements join.InfraProvider for GCE instances.
func (Provider) InfraValues(ctx context.Context, gcpMachine *unstructured.Unstructured) (map[string]any, error) {
	return map[string]any{
		"providerIdentityScript":     base64.StdEncoding.EncodeToString([]byte(BootstrapIdentityScript)),
		"providerIdentityPowerShell": base64.StdEncoding.EncodeToString([]byte(BootstrapIdentityPowerShell)),
	}, nil
}
