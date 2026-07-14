// Package aws implements join.InfraProvider for CAPA (cluster-api-provider-aws).
package aws

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Provider implements join.InfraProvider for AWSMachine. Reads status
// fields directly off the unstructured Machine's infrastructureRef
// target rather than importing CAPA's own types, matching how
// endpoint-controller already avoids depending on AWSMachine's schema.
//
// AWSMachine's own creation is entirely CAPA's job (a separate
// operator this reconciler has only a soft/graceful dependency on,
// see isMissingCRD in pkg/join/reconciler.go) -- this Provider only
// ever reads it, never creates or drives it.
type Provider struct{}

var gvk = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSMachine"}

// GVK implements join.InfraProvider.
func (Provider) GVK() schema.GroupVersionKind { return gvk }

// Ready reports whether the underlying AWSMachine is far enough along
// to bootstrap: status.ready is true. A future GCPProvider would check
// whatever its own infrastructure CRD's status shape happens to be.
func (Provider) Ready(ctx context.Context, awsMachine *unstructured.Unstructured) (bool, error) {
	ready, found, err := unstructured.NestedBool(awsMachine.Object, "status", "ready")
	if err != nil {
		return false, fmt.Errorf("reading status.ready: %w", err)
	}
	return found && ready, nil
}

// InfraValues contributes nothing today -- the join-pattern template
// doesn't need any AWS-specific fact beyond "is it ready yet" (that's
// what gates whether the reconciler runs at all). Kept as a real
// method (not omitted) so the interface stays honest about what a
// future infra provider could contribute.
func (Provider) InfraValues(ctx context.Context, awsMachine *unstructured.Unstructured) (map[string]any, error) {
	return map[string]any{}, nil
}
