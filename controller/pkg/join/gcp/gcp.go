// Package gcp implements join.InfraProvider and join.MachineProvisioner
// for CAPG (cluster-api-provider-gcp), enabling Google Cloud Compute Engine
// worker machines alongside AWS, Docker, and Labcontainers in the same cluster.
package gcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Provider supplies CAPG identity, preflight validation, and GCE instance
// defaults (publicIP, stock Ubuntu imageFamily fallback, and a startup-script
// bridge so stock GCE Debian/RHEL images without cloud-init still execute
// CAPG's user-data #cloud-config).
type Provider struct {
	NetworkTag string
}

var (
	// CAPG (cluster-api-provider-gcp) serves infrastructure.cluster.x-k8s.io/v1beta1.
	gvk           = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta1", Kind: "GCPMachine"}
	gcpClusterGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta1", Kind: "GCPCluster"}
	clusterGVK    = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}
)

const (
	clusterNameLabel = "cluster.x-k8s.io/cluster-name"
	// DefaultImageFamilyAMD64 is the official GCP Ubuntu 24.04 LTS amd64 image
	// family (includes cloud-init out of the box). Used when neither spec.image
	// nor spec.imageFamily is set on GCPMachine so CAPG does not fall back to a
	// non-existent project-local "capi-ubuntu-1804-k8s-*" image family.
	DefaultImageFamilyAMD64 = "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"
	// DefaultImageFamilyARM64 is the official GCP Ubuntu 24.04 LTS arm64 image family.
	DefaultImageFamilyARM64 = "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-arm64"
)

// BootstrapStartupScript is injected into GCPMachine.spec.additionalMetadata
// under key "startup-script" when not already set.
//
// Why this exists: CAPG injects Machine.spec.bootstrap.dataSecretName into the
// GCE instance metadata key "user-data" (#cloud-config). Official GCP Ubuntu
// images run cloud-init by default and consume "user-data" natively, but
// Google's default GCE OS (debian-cloud/debian-12) and RHEL/Rocky GCE images do
// NOT have cloud-init installed by default — they only run google-guest-agent,
// which executes "startup-script" and ignores "user-data". This idempotent
// bridge is a no-op when cloud-init is already present, and otherwise installs
// cloud-init via apt-get/dnf/yum and runs it against CAPG's user-data.
const BootstrapStartupScript = `#!/bin/sh
set -eu
if [ -f /run/cluster-api/bootstrap-success.complete ]; then
  exit 0
fi
if command -v cloud-init >/dev/null 2>&1; then
  exit 0
fi
if command -v apt-get >/dev/null 2>&1; then
  DEBIAN_FRONTEND=noninteractive apt-get update -q
  DEBIAN_FRONTEND=noninteractive apt-get install -y -q cloud-init
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y -q cloud-init
elif command -v yum >/dev/null 2>&1; then
  yum install -y -q cloud-init
fi
cloud-init init
cloud-init modules --mode=config
cloud-init modules --mode=final
`

// GVK implements join.InfraProvider.
func (Provider) GVK() schema.GroupVersionKind { return gvk }

// ClusterGVK implements join.MachineProvisioner.
func (Provider) ClusterGVK() schema.GroupVersionKind { return gcpClusterGVK }

// PrepareInfraMachine implements join.InfraMachinePreparer for GCPMachine.
func (p Provider) PrepareInfraMachine(gcpMachine *unstructured.Unstructured) (string, error) {
	if gcpMachine == nil {
		return "", nil
	}
	var failureDomain string
	if z, found, _ := unstructured.NestedString(gcpMachine.Object, "spec", "zone"); found && z != "" {
		failureDomain = z
		unstructured.RemoveNestedField(gcpMachine.Object, "spec", "zone")
	}
	if fd, found, _ := unstructured.NestedString(gcpMachine.Object, "spec", "failureDomain"); found && fd != "" {
		if failureDomain == "" {
			failureDomain = fd
		}
		unstructured.RemoveNestedField(gcpMachine.Object, "spec", "failureDomain")
	}
	spec, _ := gcpMachine.Object["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		gcpMachine.Object["spec"] = spec
	}
	arch := gcpMachine.GetLabels()["cloud-provisioning.appmana.com/arch"]
	if err := PrepareGCPMachineSpec(spec, arch, p.NetworkTag); err != nil {
		return "", err
	}
	return failureDomain, nil
}

// PrepareGCPMachineSpec populates defaults on a GCPMachine spec (or
// GCPMachineTemplate.spec.template.spec):
//   - publicIP: true
//   - imageFamily: projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-<arch>
//   - additionalMetadata: startup-script cloud-init bridge for Debian/RHEL
//   - additionalNetworkTags: optional WireGuard firewall network tag
func PrepareGCPMachineSpec(spec map[string]any, archLabel, networkTag string) error {
	if spec == nil {
		return nil
	}
	if _, found := spec["publicIP"]; !found {
		spec["publicIP"] = true
	}
	img, _ := spec["image"].(string)
	imgFamily, _ := spec["imageFamily"].(string)
	if strings.TrimSpace(img) == "" && strings.TrimSpace(imgFamily) == "" {
		instType, _ := spec["instanceType"].(string)
		defaultFamily := DefaultImageFamilyAMD64
		if isARM64InstanceType(instType) || archLabel == "arm64" {
			defaultFamily = DefaultImageFamilyARM64
		}
		spec["imageFamily"] = defaultFamily
	}
	items, _ := spec["additionalMetadata"].([]any)
	hasStartupScript := false
	for _, raw := range items {
		if m, ok := raw.(map[string]any); ok && m["key"] == "startup-script" {
			hasStartupScript = true
			break
		}
	}
	if !hasStartupScript {
		items = append(items, map[string]any{
			"key":   "startup-script",
			"value": BootstrapStartupScript,
		})
		spec["additionalMetadata"] = items
	}
	if tag := strings.TrimSpace(networkTag); tag != "" {
		tags, _ := spec["additionalNetworkTags"].([]any)
		hasTag := false
		for _, t := range tags {
			if ts, ok := t.(string); ok && ts == tag {
				hasTag = true
				break
			}
		}
		if !hasTag {
			spec["additionalNetworkTags"] = append(tags, tag)
		}
	}
	return nil
}

func isARM64InstanceType(instanceType string) bool {
	prefix := strings.SplitN(strings.ToLower(strings.TrimSpace(instanceType)), "-", 2)[0]
	return prefix == "t2a" || prefix == "c4a" || prefix == "n4a" || prefix == "a4x"
}

// Validate implements join.Validator: traces a GCPMachine to its Cluster and
// GCPCluster, and when spec.credentialsRef is set, verifies that the referenced
// Secret exists and contains the "credentials" key required by CAPG.
func (Provider) Validate(ctx context.Context, c client.Reader, gcpMachine *unstructured.Unstructured) error {
	clusterName := gcpMachine.GetLabels()[clusterNameLabel]
	if clusterName == "" {
		return nil
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: gcpMachine.GetNamespace(), Name: clusterName}, cluster); err != nil {
		return nil
	}
	infraRefKind, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "kind")
	infraRefName, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "name")
	if infraRefKind != "GCPCluster" || infraRefName == "" {
		return nil
	}

	gcpCluster := &unstructured.Unstructured{}
	gcpCluster.SetGroupVersionKind(gcpClusterGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: cluster.GetNamespace(), Name: infraRefName}, gcpCluster); err != nil {
		// Also try v1beta2 in case a future CAPG CRD version is installed.
		gcpCluster.SetGroupVersionKind(schema.GroupVersionKind{Group: gcpClusterGVK.Group, Version: "v1beta2", Kind: "GCPCluster"})
		if err2 := c.Get(ctx, client.ObjectKey{Namespace: cluster.GetNamespace(), Name: infraRefName}, gcpCluster); err2 != nil {
			return nil
		}
	}
	secretName, _, _ := unstructured.NestedString(gcpCluster.Object, "spec", "credentialsRef", "name")
	if secretName == "" {
		return nil
	}
	secretNamespace, _, _ := unstructured.NestedString(gcpCluster.Object, "spec", "credentialsRef", "namespace")
	if secretNamespace == "" {
		secretNamespace = gcpCluster.GetNamespace()
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: secretNamespace, Name: secretName}, secret); err != nil {
		return fmt.Errorf("GCPCluster %q references credentialsRef Secret %s/%s, which does not exist: %w", infraRefName, secretNamespace, secretName, err)
	}
	if len(secret.Data["credentials"]) == 0 && secret.StringData["credentials"] == "" {
		return fmt.Errorf("GCPCluster %q credentialsRef Secret %s/%s is missing required key \"credentials\" (CAPG reads Secret.data[\"credentials\"])", infraRefName, secretNamespace, secretName)
	}
	return nil
}

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
