// Package join defines the abstractions a bootstrap-provisioning
// reconciler composes to turn a bare Machine into a rendered,
// ready-to-apply cloud-init bootstrap Secret -- without assuming any
// particular cluster technology or infrastructure provider. k0s and
// AWS are the first concrete implementations, not the only ones this
// is designed for.
package join

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ClusterJoinProvider is however a specific cluster technology (k0s,
// kubeadm, k3s, ...) grants a new node whatever it needs to join.
// Different technologies produce genuinely different shapes of
// credential -- a bare token, a bootstrap-token/CA-hash pair, a
// discovery URL -- so this returns an opaque values map fed straight
// into the join-pattern template (render-join-data already treats its
// input as a generic YAML values map, not a fixed schema) rather than
// forcing every implementation to look like "a token string".
type ClusterJoinProvider interface {
	// JoinValues returns the template values this cluster technology
	// contributes for one new node (e.g. a k0s implementation returns
	// {"joinToken": "...", "k0sVersion": "..."}).
	JoinValues(ctx context.Context) (map[string]any, error)
}

// InfraProvider is however a specific infrastructure provider (AWS,
// GCP, ...) knows whether one Machine's underlying resource is ready
// to be bootstrapped yet, and contributes whatever placement/identity
// facts about it the join-pattern template needs. Readiness is
// genuinely provider-specific: each infrastructure CRD (AWSMachine,
// a hypothetical GCPMachine, ...) has its own status shape.
type InfraProvider interface {
	// Ready reports whether the Machine's underlying infrastructure
	// resource is far enough along to bootstrap (e.g. AWS: the
	// instance is running). False, nil means "not yet, requeue" --
	// not an error.
	Ready(ctx context.Context, machine *unstructured.Unstructured) (bool, error)

	// InfraValues returns this provider's template value contribution
	// for one Machine (e.g. AWS might contribute nothing today, but a
	// future provider could contribute its own placement facts).
	InfraValues(ctx context.Context, machine *unstructured.Unstructured) (map[string]any, error)
}
