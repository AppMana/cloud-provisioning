// Package aws implements join.InfraProvider and join.MachineProvisioner
// for CAPA (cluster-api-provider-aws).
package aws

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Provider reads/renders unstructured CAPA objects rather than
// importing CAPA's own types, matching how the rest of this module
// avoids depending on AWSMachine's schema. AWSMachine's actual
// reconciliation (RunInstances etc.) is entirely CAPA's job -- this
// Provider renders specs and reads status, never drives AWS itself.
//
// ConfigNamespace/ConfigName point at a plain Secret carrying this
// provider's cluster-level configuration -- the place AWS specifics
// live so that ProvisionedNodeClaims never have to (see
// join.MachineProvisioner). Keys:
//
//	ami-arm64, ami-amd64        -- per-arch AMI IDs (at least the arch
//	                               in use is required)
//	ssh-key-name                -- optional EC2 keypair name
//	security-group-ids          -- optional comma-separated sg-...
//	subnet-id                   -- optional (CAPA picks from the
//	                               AWSCluster otherwise)
//	iam-instance-profile        -- optional
//	public-ip                   -- "true"/"false" (default true: the
//	                               whole point of these nodes)
//	insecure-skip-secrets-manager -- "true"/"false" (default true:
//	                               userdata already carries only a
//	                               short-TTL join token; SSM adds an
//	                               IAM dependency for no secret kept)
type Provider struct {
	ConfigNamespace string
	ConfigName      string
}

var (
	gvk        = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSMachine"}
	clusterGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}

	awsClusterGVK               = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSCluster"}
	awsClusterStaticIdentityGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSClusterStaticIdentity"}
)

// GVK implements join.InfraProvider.
func (Provider) GVK() schema.GroupVersionKind { return gvk }

// ClusterGVK implements join.MachineProvisioner.
func (Provider) ClusterGVK() schema.GroupVersionKind { return awsClusterGVK }

// InfraValues contributes nothing beyond what the machine's own spec
// already says. Architecture used to be derived here to pick a
// download; the machine reads its own at boot instead.
func (Provider) InfraValues(ctx context.Context, awsMachine *unstructured.Unstructured) (map[string]any, error) {
	return map[string]any{}, nil
}

// InfraMachine implements join.MachineProvisioner: renders the
// AWSMachine spec for one claim from the provider-config Secret.
// managerNamespace is where CAPA ALWAYS resolves an
// AWSClusterStaticIdentity's secretRef, regardless of where the
// AWSCluster/AWSMachine themselves live -- confirmed directly from the
// installed version's source (cluster-api-provider-aws@v2.12.1,
// pkg/cloud/scope/session.go's buildAWSClusterStaticIdentity calling
// system.GetManagerNamespace(), which reads the CAPA pod's own
// in-cluster namespace file/POD_NAMESPACE, defaulting to
// "capa-system"). Hardcoded to match this specific installation, not
// discovered dynamically -- there's exactly one CAPA install per
// cluster, in a namespace fixed by its manifest.
const managerNamespace = "capa-system"

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
// genuinely out of this check's scope. Only the one specific,
// confirmed-real misconfiguration this check exists for is surfaced.
func (Provider) Validate(ctx context.Context, c client.Reader, awsMachine *unstructured.Unstructured) error {
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
