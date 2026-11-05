package generic

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestGenericProviderKindMappings(t *testing.T) {
	p := Provider{}
	if !p.MatchesMachineKind("infrastructure.cluster.x-k8s.io", "AzureMachine") {
		t.Fatal("expected AzureMachine to match generic CAPI provider")
	}
	if !p.MatchesClusterKind("infrastructure.cluster.x-k8s.io", "AzureCluster", "AzureMachineTemplate") {
		t.Fatal("expected AzureCluster + AzureMachineTemplate to match generic CAPI provider")
	}
	if !p.MatchesClusterKind("infrastructure.cluster.x-k8s.io", "HetznerCluster", "HetznerBareMetalMachineTemplate") {
		t.Fatal("expected HetznerCluster + HetznerBareMetalMachineTemplate to match generic CAPI provider")
	}
	if p.MatchesClusterKind("infrastructure.cluster.x-k8s.io", "AzureCluster", "OpenStackMachineTemplate") {
		t.Fatal("mismatched cluster and template kinds must not match")
	}

	bound := p.ForClusterAndTemplate(nil, "infrastructure.cluster.x-k8s.io", "AzureCluster", "infrastructure.cluster.x-k8s.io", "AzureMachineTemplate")
	if bound.GVK() != (schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AzureMachine"}) {
		t.Fatalf("unexpected bound Machine GVK: %+v", bound.GVK())
	}
	if bound.ClusterGVK() != (schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AzureCluster"}) {
		t.Fatalf("unexpected bound Cluster GVK: %+v", bound.ClusterGVK())
	}
}

func TestGenericProviderInfraValues(t *testing.T) {
	p := Provider{}
	// 1. Dynamic cloud machine without pre-populated spec.providerID gets metadata script.
	emptyMachine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "AzureMachine",
	}}
	vals, err := p.InfraValues(context.Background(), emptyMachine)
	if err != nil {
		t.Fatalf("InfraValues: %v", err)
	}
	b64, ok := vals["providerIdentityScript"].(string)
	if !ok || b64 == "" {
		t.Fatal("expected providerIdentityScript when spec.providerID is unset")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || !strings.Contains(string(raw), "azure://") || !strings.Contains(string(raw), "openstack:///") {
		t.Fatalf("unexpected script content: %s (err=%v)", string(raw), err)
	}

	// 2. Pre-populated spec.providerID and status.addresses are forwarded directly.
	prepopulated := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "VSphereMachine",
		"spec": map[string]any{
			"providerID": "vsphere://42307a29-1234-5678-9abc-def012345678",
		},
		"status": map[string]any{
			"addresses": []any{
				map[string]any{"type": "InternalIP", "address": "10.20.30.40"},
			},
		},
	}}
	vals2, err := p.InfraValues(context.Background(), prepopulated)
	if err != nil {
		t.Fatalf("InfraValues(prepopulated): %v", err)
	}
	if vals2["providerID"] != "vsphere://42307a29-1234-5678-9abc-def012345678" {
		t.Fatalf("providerID = %v", vals2["providerID"])
	}
	if vals2["nodeAddress"] != "10.20.30.40" {
		t.Fatalf("nodeAddress = %v", vals2["nodeAddress"])
	}
	if _, hasScript := vals2["providerIdentityScript"]; hasScript {
		t.Fatal("expected no providerIdentityScript when static providerID is already present")
	}
}
