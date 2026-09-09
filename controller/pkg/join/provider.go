// Package join defines the abstractions a bootstrap-provisioning
// reconciler composes to turn a bare Machine into a rendered,
// native guest bootstrap Secret, without assuming any
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

// MachineProvisioner identifies the infrastructure cluster kind a provider
// supports. Machine shape comes from the claim's infrastructure template.
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

// NodeLocalBalancer is an optional capability a ClusterJoinProvider
// implements when its distribution ships no node-local balancing of
// its own, so a joining worker needs the operator's loopback balancer
// (wg-apiproxy) to avoid depending on any single control plane.
//
// This is the one place the who-balances decision lives in code; the
// table in join-patterns/README.md is its record. kubeadm implements
// it (kubelet would otherwise dial the join endpoint forever); k0s
// (nllb), k3s and RKE2 (the agent's client-side balancer) do not, and
// their absence here is what keeps apiProxyPort zero in their renders,
// so the shared pattern blocks emit no balancer unit at all. A second
// balancer stacked on a distribution's own is not redundancy, it is
// two owners for one address.
type NodeLocalBalancer interface {
	NeedsAPIProxy() bool
}

// AddressObserver is an optional capability an InfraProvider
// implements when it observes machines that already exist, so it
// knows where one is before that machine is bootstrapped.
//
// It decides whether rendering may proceed without an address. A
// provider that creates the instance cannot know it: CAPA does not
// call RunInstances until the bootstrap Secret exists, so waiting for
// an address there deadlocks, and the instance has to ask its own
// metadata service once it is running. A provider that adopts a
// machine already has the answer, and rendering before it arrives
// bakes a document that tells the node nothing — the same failure as
// baking an empty peer list, and for the same reason: userdata is
// read once.
//
// The cost of getting it wrong is not a failed join. The node comes
// up, chooses an address for itself, and picks the wrong one wherever
// it has more than one; it then joins, goes Ready, and carries an
// identity nothing is looking for.
type AddressObserver interface {
	ObservesAddresses() bool
}
