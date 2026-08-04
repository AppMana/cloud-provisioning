// Package join defines the abstractions a bootstrap-provisioning
// reconciler composes to turn a bare Machine into a rendered,
// ready-to-apply cloud-init bootstrap Secret, without assuming any
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
// Different technologies produce different shapes of credential (a
// bare token, a bootstrap-token/CA-hash pair, a discovery URL), so
// this returns an opaque values map fed straight
// into the join-pattern template (pkg/render treats its input as a
// generic values map, not a fixed schema) rather than forcing every
// implementation to look like "a token string".
type ClusterJoinProvider interface {
	// JoinValues returns the template values this cluster technology
	// contributes for one new node (e.g. a k0s implementation returns
	// {"joinToken": "...", "k0sVersion": "..."}).
	JoinValues(ctx context.Context) (map[string]any, error)
}

// InfraProvider is however a specific infrastructure provider (AWS, a
// Docker-backed test double, GCP, ...) contributes whatever
// placement/identity facts about one Machine the join-pattern
// template needs.
//
// The reconciler does not hardcode which provider applies to a given
// Machine. It infers that from the Machine's own
// spec.infrastructureRef.kind, matching it against each registered
// provider's GVK(). Adding a new infrastructure provider means
// registering it, not adding a branch to the reconciler.
type InfraProvider interface {
	// GVK identifies the infrastructure resource kind this provider
	// handles (e.g. AWSMachine at infrastructure.cluster.x-k8s.io/v1beta2).
	// The reconciler matches this against a Machine's
	// spec.infrastructureRef.kind to pick the right provider, and uses
	// it to know which object to fetch.
	GVK() schema.GroupVersionKind

	// InfraValues returns this provider's template value contribution
	// for one Machine (e.g. AWS contributes "arch", derived from the
	// instance type, which selects the dialer binary the userdata
	// downloads).
	InfraValues(ctx context.Context, machine *unstructured.Unstructured) (map[string]any, error)
}

// NodeRequest is the provider-agnostic ask a ProvisionedNodeClaim
// boils down to: how much compute, which architecture, whether the
// node is internet-facing. It carries no cloud-specific field: which
// cloud fulfills it, and with what instance type, is the fulfilling
// provider's business.
type NodeRequest struct {
	CPUMillis      int64
	MemoryBytes    int64
	Arch           string
	InternetFacing bool
}

// MachineProvisioner is the capability a provider implements to
// fulfill ProvisionedNodeClaims: resolving a NodeRequest to one of
// its own instance types (smallest fit from its own catalog; this is
// not a scheduler), and rendering the provider-specific machine object
// from its cluster-level config. Providers that only support
// pre-authored Machines (test doubles) do not implement it.
type MachineProvisioner interface {
	InfraProvider

	// ClusterGVK identifies the provider's cluster-scoped
	// infrastructure kind (e.g. AWSCluster). The claim reconciler uses
	// it to route a claim to the provider that owns the CAPI Cluster's
	// infrastructure.
	ClusterGVK() schema.GroupVersionKind
}

// Validator is an optional capability an InfraProvider may implement:
// a Reconcile-time preflight check that surfaces a misconfiguration in
// whatever separate operator owns provisioning (CAPA, ...) as an
// immediate, clear error from this reconciler, instead of an opaque,
// indefinite retry loop in that other operator's logs. For example,
// CAPA retries indefinitely with "Secret ... not found" when an
// AWSClusterStaticIdentity's secretRef is in the wrong namespace, a
// configuration mistake this reconciler can catch early because it
// already resolves the infrastructure object.
//
// It is not part of InfraProvider itself: it is optional and
// provider-specific (containernet, for instance, has nothing
// analogous to validate), so callers type-assert for it rather than
// every provider implementing a no-op. Validate takes a Reader for the
// same reason InfraMachine does: it only reads, and the cached
// client's typed Gets would otherwise start cluster-wide informers
// needing RBAC this identity does not have.
type Validator interface {
	Validate(ctx context.Context, c client.Reader, infraMachine *unstructured.Unstructured) error
}
