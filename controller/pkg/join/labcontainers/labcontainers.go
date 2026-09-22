// Package labcontainers supplies infrastructure values for Labcontainers'
// Cluster API contract.
package labcontainers

import (
	"context"

	labcapi "github.com/appmana/labcontainers/pkg/capi"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	machineGVK = schema.GroupVersionKind{Group: labcapi.Group, Version: labcapi.Version, Kind: labcapi.MachineKind}
	clusterGVK = schema.GroupVersionKind{Group: labcapi.Group, Version: labcapi.Version, Kind: labcapi.ClusterKind}
)

// Provider implements the join infrastructure and machine-provisioner seams.
type Provider struct{}

func (Provider) GVK() schema.GroupVersionKind        { return machineGVK }
func (Provider) ClusterGVK() schema.GroupVersionKind { return clusterGVK }
func (Provider) ObservesAddresses() bool             { return true }

func (Provider) InfraValues(_ context.Context, machine *unstructured.Unstructured) (map[string]any, error) {
	return labcapi.InfraValues(machine.Object), nil
}
