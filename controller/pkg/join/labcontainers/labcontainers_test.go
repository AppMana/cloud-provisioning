package labcontainers

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Pin the kubelet to the address reported by the infrastructure provider.
// On a multi-interface machine, automatic selection can choose an unrelated
// management address and prevent association with the provider's Machine.
func TestInfraValuesTellTheNodeItsOwnAddress(t *testing.T) {
	machine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "LabMachine",
		"metadata":   map[string]any{"name": "remote1", "namespace": "default"},
		"status": map[string]any{
			"addresses": []any{
				map[string]any{"type": "ExternalIP", "address": "203.0.113.10"},
				map[string]any{"type": "InternalIP", "address": "203.0.113.10"},
			},
		},
	}}

	values, err := Provider{}.InfraValues(context.Background(), machine)
	if err != nil {
		t.Fatal(err)
	}
	if values["nodeAddress"] != "203.0.113.10" {
		t.Errorf("the provider reports 203.0.113.10 and tells the node %q, "+
			"so the kubelet is left to pick from every interface it has", values["nodeAddress"])
	}
}

// With no address reported there is nothing to say, and saying
// nothing has to leave the node exactly as it was: free to work it
// out however its distribution does.
func TestInfraValuesSayNothingWhenNoAddressIsReported(t *testing.T) {
	machine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "LabMachine",
		"metadata":   map[string]any{"name": "remote1", "namespace": "default"},
	}}

	values, err := Provider{}.InfraValues(context.Background(), machine)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := values["nodeAddress"]; ok && got != "" {
		t.Errorf("an address was invented from nothing: %q", got)
	}
}

// The provider's identity for a machine reaches the node, because
// Cluster API binds the two by it.
//
// Some distributions assign one themselves. k3s gives every node
// k3s://<name> as it registers, and RKE2 does the same, so a node
// that is not told otherwise ends up carrying an identity the Machine
// does not have: spec.providerID says labcontainers://cldt/remote1, the
// node says k3s://remote1, status.nodeRef is never set, and the
// controller that publishes the remote's pod block never finds a node
// to publish for. The remote joins, goes Ready, and nothing at the
// site can reach a pod on it, with every component reporting healthy.
func TestInfraValuesCarryTheProviderIdentity(t *testing.T) {
	machine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "LabMachine",
		"metadata":   map[string]any{"name": "remote1", "namespace": "default"},
		"spec":       map[string]any{"providerID": "labcontainers://cldt/remote1"},
	}}

	values, err := Provider{}.InfraValues(context.Background(), machine)
	if err != nil {
		t.Fatal(err)
	}
	if values["providerID"] != "labcontainers://cldt/remote1" {
		t.Errorf("the node is told %q, so a distribution that names itself wins and "+
			"Cluster API never binds the Machine to it", values["providerID"])
	}
}

// With none assigned yet there is nothing to say, and the node keeps
// whatever its distribution or cloud provider gives it.
func TestInfraValuesSayNothingWithoutAProviderIdentity(t *testing.T) {
	machine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "LabMachine",
		"metadata":   map[string]any{"name": "remote1", "namespace": "default"},
	}}
	values, err := Provider{}.InfraValues(context.Background(), machine)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := values["providerID"]; ok && got != "" {
		t.Errorf("an identity was invented from nothing: %q", got)
	}
}
