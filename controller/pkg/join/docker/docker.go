// Package docker implements join.InfraProvider and
// join.MachineProvisioner for CAPD (the Cluster API Docker provider,
// cluster-api's own development/test infrastructure). AWS is one
// implementation of machine fulfillment; this is another over the same
// seams, and it makes a plain kind cluster a complete local e2e of
// the single-resource flow: a ProvisionedNodeClaim against a DockerCluster
// resolves here, CAPD launches the "cloud" node as a container and
// executes the rendered cloud-config bootstrap, and the node joins the
// kind control plane for real. No QEMU, no bespoke cluster rig.
package docker

import (
	"context"
	"runtime"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Provider supplies CAPD identity and bootstrap template values. Machine shape
// is copied from a DockerMachineTemplate by the claim reconciler.
type Provider struct{}

var (
	// v1beta2, matching the cluster.x-k8s.io era this module verifies
	// against live; the e2e that installs CAPD asserts the installed
	// CRD actually serves this version.
	gvk              = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "DockerMachine"}
	dockerClusterGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "DockerCluster"}
)

// GVK implements join.InfraProvider.
func (Provider) GVK() schema.GroupVersionKind { return gvk }

// ClusterGVK implements join.MachineProvisioner.
func (Provider) ClusterGVK() schema.GroupVersionKind { return dockerClusterGVK }

// InfraValues implements join.InfraProvider: a container runs whatever
// the host's architecture is.
func (Provider) InfraValues(ctx context.Context, machine *unstructured.Unstructured) (map[string]any, error) {
	return map[string]any{"arch": runtime.GOARCH}, nil
}
