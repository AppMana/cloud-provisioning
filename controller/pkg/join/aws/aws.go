// Package aws implements join.InfraProvider for CAPA (cluster-api-provider-aws).
package aws

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// managerNamespace is where CAPA ALWAYS resolves an
// AWSClusterStaticIdentity's secretRef, regardless of where the
// AWSCluster/AWSMachine themselves live -- confirmed directly from the
// installed version's source (cluster-api-provider-aws@v2.12.1,
// pkg/cloud/scope/session.go's buildAWSClusterStaticIdentity calling
// system.GetManagerNamespace(), which reads the CAPA pod's own
// in-cluster namespace file/POD_NAMESPACE, defaulting to
// "capa-system"). Hardcoded to match this specific installation
// (infrastructure/base/cluster-api-providers/providers.yaml), not
// discovered dynamically -- there's exactly one CAPA install in this
// cluster, in a namespace fixed by that manifest.
const managerNamespace = "capa-system"

var (
	clusterGVK                  = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}
	awsClusterGVK               = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSCluster"}
	awsClusterStaticIdentityGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSClusterStaticIdentity"}
)

const clusterNameLabel = "cluster.x-k8s.io/cluster-name"

// Validate implements join.Validator: traces an AWSMachine up to its
// Cluster -> AWSCluster -> identityRef, and -- when that identity is
// an AWSClusterStaticIdentity -- confirms its secretRef Secret
// actually exists in managerNamespace. A misplaced Secret produces an
// indefinite, opaque CAPA retry loop ("Secret ... not found") with no
// actionable signal about WHERE it needs to be; this turns that into
// an immediate, clear error instead.
//
// Every traversal step before the final Secret check fails open (nil,
// not an error): a missing Cluster/AWSCluster, an unset or
// non-static-identity identityRef, or an unresolvable
// AWSClusterStaticIdentity are all either "nothing to validate yet" or
// genuinely out of this check's scope -- CAPA's own error handling
// (including this reconciler's isMissingCRD graceful requeue) already
// covers those. Only the one specific, confirmed-real misconfiguration
// this check exists for is surfaced as an error.
func (Provider) Validate(ctx context.Context, c client.Client, awsMachine *unstructured.Unstructured) error {
	clusterName := awsMachine.GetLabels()[clusterNameLabel]
	if clusterName == "" {
		return nil
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: awsMachine.GetNamespace(), Name: clusterName}, cluster); err != nil {
		return nil
	}
	infraRefKind, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "kind")
	infraRefName, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "name")
	if infraRefKind != "AWSCluster" || infraRefName == "" {
		return nil
	}

	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(awsClusterGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: cluster.GetNamespace(), Name: infraRefName}, awsCluster); err != nil {
		return nil
	}
	identityRefKind, _, _ := unstructured.NestedString(awsCluster.Object, "spec", "identityRef", "kind")
	identityRefName, _, _ := unstructured.NestedString(awsCluster.Object, "spec", "identityRef", "name")
	if identityRefKind != "AWSClusterStaticIdentity" || identityRefName == "" {
		return nil
	}

	identity := &unstructured.Unstructured{}
	identity.SetGroupVersionKind(awsClusterStaticIdentityGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: identityRefName}, identity); err != nil {
		return nil
	}
	secretRefName, _, _ := unstructured.NestedString(identity.Object, "spec", "secretRef")
	if secretRefName == "" {
		return nil
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: managerNamespace, Name: secretRefName}, secret); err != nil {
		return fmt.Errorf(
			"AWSClusterStaticIdentity %q references secretRef %q, but CAPA always resolves it against its own manager namespace %q, not %q (where the AWSCluster lives) -- create/move the Secret to namespace %q: %w",
			identityRefName, secretRefName, managerNamespace, awsCluster.GetNamespace(), managerNamespace, err,
		)
	}
	return nil
}
