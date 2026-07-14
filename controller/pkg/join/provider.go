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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
// a containernet-backed test double, GCP, ...) knows whether one
// Machine's underlying resource is ready to be bootstrapped yet, and
// contributes whatever placement/identity facts about it the
// join-pattern template needs. Readiness is genuinely
// provider-specific: each infrastructure CRD (AWSMachine, a
// ContainernetMachine test double, a hypothetical GCPMachine, ...)
// has its own status shape.
//
// The reconciler never hardcodes which provider applies to a given
// Machine -- it infers that from the Machine's own
// spec.infrastructureRef.kind (a real, already-present field, not
// invented for this), matching it against each registered provider's
// GVK(). Adding a new infrastructure provider means registering it,
// never adding a branch to the reconciler.
type InfraProvider interface {
	// GVK identifies the infrastructure resource kind this provider
	// handles (e.g. AWSMachine at infrastructure.cluster.x-k8s.io/v1beta2).
	// The reconciler matches this against a Machine's
	// spec.infrastructureRef.kind to pick the right provider, and uses
	// it to know which object to fetch.
	GVK() schema.GroupVersionKind

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

// Validator is an optional capability an InfraProvider may implement:
// a Reconcile-time preflight check that surfaces a misconfiguration in
// whatever *separate* operator actually owns provisioning (CAPA, ...)
// as an immediate, clear error from THIS reconciler, instead of an
// opaque, indefinite retry loop in that other operator's own logs.
// Caught live: CAPA silently retried forever with "Secret ... not
// found" because an AWSClusterStaticIdentity's secretRef was in the
// wrong namespace -- a real, non-obvious configuration mistake this
// reconciler is well-placed to catch early, since it already resolves
// the infrastructure object anyway.
//
// Deliberately not part of InfraProvider itself: it's genuinely
// optional and provider-specific (containernet, for instance, has
// nothing analogous to validate), so callers type-assert for it
// rather than every provider being forced to implement a no-op.
type Validator interface {
	Validate(ctx context.Context, c client.Client, infraMachine *unstructured.Unstructured) error
}
