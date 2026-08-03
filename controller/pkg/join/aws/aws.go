// Package aws implements join.InfraProvider and join.MachineProvisioner
// for CAPA (cluster-api-provider-aws).
package aws

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/join"
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

// arm64Families are the EC2 instance families this catalog knows to be
// Graviton (arm64). Everything else is treated as amd64.
var arm64Families = map[string]bool{
	"t4g": true, "m6g": true, "m7g": true, "m8g": true,
	"c6g": true, "c7g": true, "c8g": true,
	"r6g": true, "r7g": true, "r8g": true,
	"a1": true, "im4gn": true, "g5g": true,
}

func archForInstanceType(instanceType string) string {
	family := strings.SplitN(instanceType, ".", 2)[0]
	if arm64Families[family] {
		return "arm64"
	}
	return "amd64"
}

// InfraValues contributes "arch", derived from the AWSMachine's
// instance type -- it selects which dialer binary the rendered
// userdata downloads.
func (Provider) InfraValues(ctx context.Context, awsMachine *unstructured.Unstructured) (map[string]any, error) {
	instanceType, _, _ := unstructured.NestedString(awsMachine.Object, "spec", "instanceType")
	if instanceType == "" {
		return map[string]any{}, nil
	}
	return map[string]any{"arch": archForInstanceType(instanceType)}, nil
}

// catalogEntry is one instance type this provider will resolve a
// NodeRequest onto. The catalog is deliberately tiny and static --
// burstable general-purpose types, the only shape a tunnel/ingress
// node needs. It is NOT a general EC2 catalog and never will be;
// anything fancier belongs to a real autoscaler, which this project
// deliberately is not.
type catalogEntry struct {
	name        string
	arch        string
	cpuMillis   int64
	memoryBytes int64
}

const gib = int64(1) << 30

var catalog = []catalogEntry{
	{"t4g.nano", "arm64", 2000, gib / 2},
	{"t4g.micro", "arm64", 2000, 1 * gib},
	{"t4g.small", "arm64", 2000, 2 * gib},
	{"t4g.medium", "arm64", 2000, 4 * gib},
	{"t4g.large", "arm64", 2000, 8 * gib},
	{"t4g.xlarge", "arm64", 4000, 16 * gib},
	{"t4g.2xlarge", "arm64", 8000, 32 * gib},
	{"t3.micro", "amd64", 2000, 1 * gib},
	{"t3.small", "amd64", 2000, 2 * gib},
	{"t3.medium", "amd64", 2000, 4 * gib},
	{"t3.large", "amd64", 2000, 8 * gib},
	{"t3.xlarge", "amd64", 4000, 16 * gib},
	{"t3.2xlarge", "amd64", 8000, 32 * gib},
}

// ResolveInstanceType implements join.MachineProvisioner: smallest
// catalog entry (by memory, then CPU) satisfying the request.
func (Provider) ResolveInstanceType(req join.NodeRequest) (string, error) {
	arch := req.Arch
	if arch == "" {
		arch = "arm64"
	}
	candidates := make([]catalogEntry, 0, len(catalog))
	for _, e := range catalog {
		if e.arch == arch && e.cpuMillis >= req.CPUMillis && e.memoryBytes >= req.MemoryBytes {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no %s instance type in the catalog satisfies cpu=%dm memory=%dMi", arch, req.CPUMillis, req.MemoryBytes/(1<<20))
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].memoryBytes != candidates[j].memoryBytes {
			return candidates[i].memoryBytes < candidates[j].memoryBytes
		}
		return candidates[i].cpuMillis < candidates[j].cpuMillis
	})
	return candidates[0].name, nil
}

// InfraMachine implements join.MachineProvisioner: renders the
// AWSMachine spec for one claim from the provider-config Secret.
func (p Provider) InfraMachine(ctx context.Context, c client.Reader, namespace, instanceType string, req join.NodeRequest) (*unstructured.Unstructured, error) {
	cfg := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: p.ConfigNamespace, Name: p.ConfigName}, cfg); err != nil {
		return nil, fmt.Errorf("reading AWS provider config %s/%s: %w", p.ConfigNamespace, p.ConfigName, err)
	}
	get := func(key string) string { return strings.TrimSpace(string(cfg.Data[key])) }

	arch := archForInstanceType(instanceType)
	ami := get("ami-" + arch)
	if ami == "" {
		return nil, fmt.Errorf("AWS provider config %s/%s has no ami-%s", p.ConfigNamespace, p.ConfigName, arch)
	}

	spec := map[string]any{
		"instanceType": instanceType,
		"ami":          map[string]any{"id": ami},
		"publicIP":     get("public-ip") != "false",
		"cloudInit": map[string]any{
			"insecureSkipSecretsManager": get("insecure-skip-secrets-manager") != "false",
		},
	}
	if v := get("ssh-key-name"); v != "" {
		spec["sshKeyName"] = v
	}
	if v := get("iam-instance-profile"); v != "" {
		spec["iamInstanceProfile"] = v
	}
	if v := get("subnet-id"); v != "" {
		spec["subnet"] = map[string]any{"id": v}
	}
	if v := get("security-group-ids"); v != "" {
		var groups []any
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				groups = append(groups, map[string]any{"id": id})
			}
		}
		spec["additionalSecurityGroups"] = groups
	}

	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	return obj, nil
}

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
