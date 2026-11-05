// Package generic implements a fallback Cluster API (CAPI) InfraProvider and
// MachineProvisioner that works with any standard CAPI infrastructure provider
// (e.g. CAPZ AzureMachine/AzureCluster, CAPO OpenStackMachine/OpenStackCluster,
// CAPV VSphereMachine/VSphereCluster, CAPH HetznerBareMetalMachine/HetznerCluster,
// DigitalOcean, Proxmox, Metal3) without requiring a bespoke Go package unless
// provider-specific CNI or preflight hooks are needed.
package generic

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// DefaultInfraGroup is Cluster API's standard infrastructure API group.
	DefaultInfraGroup = "infrastructure.cluster.x-k8s.io"
	// DefaultInfraVersion is the default CAPI contract version when no RESTMapper is available.
	DefaultInfraVersion = "v1beta2"
	// ProviderIDAnnotation allows an operator or template to supply an explicit
	// providerID pattern when the infrastructure provider does not pre-populate spec.providerID.
	ProviderIDAnnotation = "cloud-provisioning.appmana.com/provider-id"
)

// Provider dynamically binds to any Cluster API <Provider>Machine and <Provider>Cluster
// resource pair following standard CAPI conventions (<Kind>Template -> <Kind>).
type Provider struct {
	MachineGVK      schema.GroupVersionKind
	InfraClusterGVK schema.GroupVersionKind
}

// GVK implements join.InfraProvider.
func (p Provider) GVK() schema.GroupVersionKind {
	if p.MachineGVK.Kind != "" {
		return p.MachineGVK
	}
	return schema.GroupVersionKind{Group: DefaultInfraGroup, Version: DefaultInfraVersion, Kind: "GenericMachine"}
}

// ClusterGVK implements join.MachineProvisioner.
func (p Provider) ClusterGVK() schema.GroupVersionKind {
	if p.InfraClusterGVK.Kind != "" {
		return p.InfraClusterGVK
	}
	return schema.GroupVersionKind{Group: DefaultInfraGroup, Version: DefaultInfraVersion, Kind: "GenericCluster"}
}

// MatchesMachineKind reports whether generic CAPI fallback can handle a Machine's
// infrastructureRef (any *Machine kind in a CAPI infrastructure group).
func (Provider) MatchesMachineKind(group, kind string) bool {
	if kind == "" || !strings.HasSuffix(kind, "Machine") {
		return false
	}
	return group == "" || strings.Contains(group, "cluster.x-k8s.io")
}

// ForMachineGVK returns a Provider bound to the given infrastructure Machine GVK.
func (p Provider) ForMachineGVK(gvk schema.GroupVersionKind) Provider {
	if gvk.Group == "" {
		gvk.Group = DefaultInfraGroup
	}
	if gvk.Version == "" {
		gvk.Version = DefaultInfraVersion
	}
	clusterKind := ClusterKindForMachineKind(gvk.Kind)
	return Provider{
		MachineGVK:      gvk,
		InfraClusterGVK: schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: clusterKind},
	}
}

// BindMachineGVK implements join.DynamicInfraProvider.
func (p Provider) BindMachineGVK(gvk schema.GroupVersionKind) join.InfraProvider {
	return p.ForMachineGVK(gvk)
}

// MatchesClusterKind reports whether generic CAPI fallback can fulfill a claim
// against a Cluster's infrastructureRef kind and a claim's MachineTemplate kind.
func (Provider) MatchesClusterKind(group, clusterKind, templateKind string) bool {
	if clusterKind == "" || !strings.HasSuffix(clusterKind, "Cluster") {
		return false
	}
	if templateKind != "" && !strings.HasSuffix(templateKind, "Template") {
		return false
	}
	if group != "" && !strings.Contains(group, "cluster.x-k8s.io") {
		return false
	}
	if templateKind != "" {
		wantCluster := ClusterKindForTemplateKind(templateKind)
		if wantCluster != "" && wantCluster != clusterKind {
			return false
		}
	}
	return true
}

// ForClusterAndTemplate returns a Provider bound to the served GVKs for the
// given CAPI infrastructure Cluster and MachineTemplate kinds.
func (p Provider) ForClusterAndTemplate(mapper apimeta.RESTMapper, clusterGroup, clusterKind, templateGroup, templateKind string) Provider {
	if clusterGroup == "" {
		clusterGroup = DefaultInfraGroup
	}
	if templateGroup == "" {
		templateGroup = clusterGroup
	}
	if clusterKind == "" && templateKind != "" {
		clusterKind = ClusterKindForTemplateKind(templateKind)
	}
	machineKind := strings.TrimSuffix(templateKind, "Template")
	if machineKind == "" || machineKind == templateKind {
		machineKind = strings.TrimSuffix(clusterKind, "Cluster") + "Machine"
	}
	machineGVK := resolveGVK(mapper, templateGroup, machineKind)
	clusterGVK := resolveGVK(mapper, clusterGroup, clusterKind)
	return Provider{
		MachineGVK:      machineGVK,
		InfraClusterGVK: clusterGVK,
	}
}

// BindClusterAndTemplate implements join.DynamicMachineProvisioner.
func (p Provider) BindClusterAndTemplate(mapper apimeta.RESTMapper, clusterGroup, clusterKind, templateGroup, templateKind string) join.MachineProvisioner {
	return p.ForClusterAndTemplate(mapper, clusterGroup, clusterKind, templateGroup, templateKind)
}

// ClusterKindForTemplateKind derives the conventional CAPI Cluster kind from a
// MachineTemplate kind (e.g. AzureMachineTemplate -> AzureCluster,
// HetznerBareMetalMachineTemplate -> HetznerCluster).
func ClusterKindForTemplateKind(templateKind string) string {
	if !strings.HasSuffix(templateKind, "Template") {
		return ""
	}
	machineKind := strings.TrimSuffix(templateKind, "Template")
	return ClusterKindForMachineKind(machineKind)
}

// ClusterKindForMachineKind derives the conventional CAPI Cluster kind from a
// Machine kind (e.g. AzureMachine -> AzureCluster, HetznerBareMetalMachine -> HetznerCluster,
// VSphereMachine -> VSphereCluster).
func ClusterKindForMachineKind(machineKind string) string {
	if !strings.HasSuffix(machineKind, "Machine") {
		return ""
	}
	prefix := strings.TrimSuffix(machineKind, "Machine")
	prefix = strings.TrimSuffix(prefix, "BareMetal")
	if prefix == "" {
		return ""
	}
	return prefix + "Cluster"
}

func resolveGVK(mapper apimeta.RESTMapper, group, kind string) schema.GroupVersionKind {
	if mapper != nil {
		if mapping, err := mapper.RESTMapping(schema.GroupKind{Group: group, Kind: kind}); err == nil && mapping.GroupVersionKind.Version != "" {
			return mapping.GroupVersionKind
		}
	}
	return schema.GroupVersionKind{Group: group, Version: DefaultInfraVersion, Kind: kind}
}

// BootstrapIdentityScript discovers the instance's CAPI providerID at first boot
// across standard cloud metadata services (explicit /etc/cloud-provisioning/provider-id,
// GCP GCE, AWS IMDSv2, Azure IMDS, OpenStack, Hetzner Cloud, DigitalOcean).
const BootstrapIdentityScript = `#!/bin/sh
set -eu
if [ -s /etc/cloud-provisioning/provider-id ]; then
  cat /etc/cloud-provisioning/provider-id
  exit 0
fi
# 1. Google Cloud (CAPG: gce://<project>/<zone>/<name>)
if project=$(curl -fsS --connect-timeout 2 --max-time 4 -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/project/project-id" 2>/dev/null) && [ -n "$project" ]; then
  zone_path=$(curl -fsS --connect-timeout 2 --max-time 4 -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/instance/zone")
  zone=${zone_path##*/}
  name=$(curl -fsS --connect-timeout 2 --max-time 4 -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/instance/name")
  printf 'gce://%s/%s/%s\n' "$project" "$zone" "$name"
  exit 0
fi
# 2. Azure (CAPZ: azure://<resourceId>)
if az_id=$(curl -fsS --connect-timeout 2 --max-time 4 -H "Metadata: true" "http://169.254.169.254/metadata/instance/compute/resourceId?api-version=2021-02-01&format=text" 2>/dev/null) && [ -n "$az_id" ]; then
  printf 'azure://%s\n' "$az_id"
  exit 0
fi
# 3. AWS EC2 IMDSv2 (CAPA: aws:///<az>/<instance-id>)
if token=$(curl -fsS --connect-timeout 2 --max-time 4 -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 60" 2>/dev/null) && [ -n "$token" ]; then
  id=$(curl -fsS --connect-timeout 2 --max-time 4 -H "X-aws-ec2-metadata-token: $token" "http://169.254.169.254/latest/meta-data/instance-id" 2>/dev/null || true)
  az=$(curl -fsS --connect-timeout 2 --max-time 4 -H "X-aws-ec2-metadata-token: $token" "http://169.254.169.254/latest/meta-data/placement/availability-zone" 2>/dev/null || true)
  if [ -n "$id" ] && [ -n "$az" ]; then
    printf 'aws:///%s/%s\n' "$az" "$id"
    exit 0
  fi
fi
# 4. Hetzner Cloud (CAPH: hcloud://<id>)
if hc_id=$(curl -fsS --connect-timeout 2 --max-time 4 "http://169.254.169.254/hetzner/v1/metadata/instance-id" 2>/dev/null) && [ -n "$hc_id" ]; then
  printf 'hcloud://%s\n' "$hc_id"
  exit 0
fi
# 5. DigitalOcean (CAPDO: digitalocean://<id>)
if do_id=$(curl -fsS --connect-timeout 2 --max-time 4 "http://169.254.169.254/metadata/v1/id" 2>/dev/null) && [ -n "$do_id" ]; then
  printf 'digitalocean://%s\n' "$do_id"
  exit 0
fi
# 6. OpenStack (CAPO: openstack:///<uuid>)
if os_meta=$(curl -fsS --connect-timeout 2 --max-time 4 "http://169.254.169.254/openstack/latest/meta_data.json" 2>/dev/null) && [ -n "$os_meta" ]; then
  os_uuid=$(printf '%s' "$os_meta" | sed -n 's/.*"uuid"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
  if [ -n "$os_uuid" ]; then
    printf 'openstack:///%s\n' "$os_uuid"
    exit 0
  fi
fi
exit 1
`

// BootstrapIdentityPowerShell discovers the instance's CAPI providerID on Windows
// across Azure, GCE, and EC2 metadata services.
const BootstrapIdentityPowerShell = `$ErrorActionPreference='Stop'
if (Test-Path 'C:\etc\cloud-provisioning\provider-id') {
  $pid = (Get-Content 'C:\etc\cloud-provisioning\provider-id' -Raw).Trim()
  $ip = (Get-NetIPAddress -AddressFamily IPv4 | Where-Object { $_.IPAddress -notlike '127.*' -and $_.IPAddress -notlike '169.254.*' } | Select-Object -First 1).IPAddress
  return @{providerID=$pid;nodeAddress=$ip}
}
try {
  $azId = (Invoke-RestMethod -Uri 'http://169.254.169.254/metadata/instance/compute/resourceId?api-version=2021-02-01&format=text' -Headers @{'Metadata'='true'} -TimeoutSec 4).Trim()
  $ip = (Invoke-RestMethod -Uri 'http://169.254.169.254/metadata/instance/network/interface/0/ipv4/ipAddress/0/privateIpAddress?api-version=2021-02-01&format=text' -Headers @{'Metadata'='true'} -TimeoutSec 4).Trim()
  if ($azId -and $ip) { return @{providerID="azure://$azId";nodeAddress=$ip} }
} catch {}
try {
  $hdr = @{'Metadata-Flavor'='Google'}
  $proj = (Invoke-RestMethod -Uri 'http://metadata.google.internal/computeMetadata/v1/project/project-id' -Headers $hdr -TimeoutSec 4).Trim()
  $zone = ((Invoke-RestMethod -Uri 'http://metadata.google.internal/computeMetadata/v1/instance/zone' -Headers $hdr -TimeoutSec 4).Trim() -split '/')[-1]
  $name = (Invoke-RestMethod -Uri 'http://metadata.google.internal/computeMetadata/v1/instance/name' -Headers $hdr -TimeoutSec 4).Trim()
  $ip = (Invoke-RestMethod -Uri 'http://metadata.google.internal/computeMetadata/v1/instance/network-interfaces/0/ip' -Headers $hdr -TimeoutSec 4).Trim()
  if ($proj -and $zone -and $name -and $ip) { return @{providerID="gce://$proj/$zone/$name";nodeAddress=$ip} }
} catch {}
throw 'Unable to determine cloud provider identity from metadata'
`

// InfraValues implements join.InfraProvider for any CAPI infrastructure machine.
// If spec.providerID or status.addresses are already populated on the object,
// they are passed directly into the bootstrap render; otherwise the multi-cloud
// metadata discovery script is supplied.
func (Provider) InfraValues(_ context.Context, machine *unstructured.Unstructured) (map[string]any, error) {
	out := map[string]any{}
	if machine != nil {
		if providerID, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID"); strings.TrimSpace(providerID) != "" {
			out["providerID"] = strings.TrimSpace(providerID)
		} else if ann := strings.TrimSpace(machine.GetAnnotations()[ProviderIDAnnotation]); ann != "" {
			out["providerID"] = ann
		}
		if addr := extractNodeAddress(machine); addr != "" {
			out["nodeAddress"] = addr
		}
	}
	if _, hasStaticID := out["providerID"]; !hasStaticID {
		out["providerIdentityScript"] = base64.StdEncoding.EncodeToString([]byte(BootstrapIdentityScript))
		out["providerIdentityPowerShell"] = base64.StdEncoding.EncodeToString([]byte(BootstrapIdentityPowerShell))
	}
	return out, nil
}

func extractNodeAddress(machine *unstructured.Unstructured) string {
	addresses, _, _ := unstructured.NestedSlice(machine.Object, "status", "addresses")
	var external string
	for _, entry := range addresses {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		addr, _ := m["address"].(string)
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		switch m["type"] {
		case "InternalIP":
			return addr
		case "ExternalIP":
			if external == "" {
				external = addr
			}
		}
	}
	return external
}
