// Package aws implements join.InfraProvider and join.MachineProvisioner
// for CAPA (cluster-api-provider-aws).
package aws

import (
	"context"
	"fmt"
	"strings"

	attachmentaws "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	platformaws "github.com/appmana/cloud-provisioning/controller/pkg/platform/aws"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultImageLookupOrg is Canonical's official AWS account ID for Ubuntu AMIs.
	DefaultImageLookupOrg = "099720109477"
	// DefaultImageLookupBaseOS is the official Ubuntu 24.04 LTS (Noble) HVM GP3 base OS path.
	DefaultImageLookupBaseOS = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04"
	// DefaultImageLookupFormatAMD64 is CAPA's imageLookupFormat for x86_64 Ubuntu AMIs.
	DefaultImageLookupFormatAMD64 = "{{.BaseOS}}-amd64-server-*"
	// DefaultImageLookupFormatARM64 is CAPA's imageLookupFormat for Graviton/ARM64 Ubuntu AMIs.
	DefaultImageLookupFormatARM64 = "{{.BaseOS}}-arm64-server-*"
	// DefaultIAMInstanceProfile is the standard clusterawsadm worker instance profile.
	DefaultIAMInstanceProfile = "nodes.cluster-api-provider-aws.sigs.k8s.io"
)

// AWSSpecDefaults holds cluster-level defaults injected into AWSMachineTemplate
// and AWSMachine specs by the mutating admission webhook and PrepareInfraMachine.
type AWSSpecDefaults struct {
	SecurityGroupIDs   []string
	SecurityGroupName  string
	IAMInstanceProfile string
	SSHKeyName         string
	VPCID              string
	SubnetID           string
	AMIAMD64           string
	AMIARM64           string
}

// Provider reads/renders unstructured CAPA objects rather than
// importing CAPA's own types, matching how the rest of this module
// avoids depending on AWSMachine's schema. AWSMachine's own
// reconciliation (RunInstances etc.) is CAPA's job; this Provider
// renders specs and reads status, and does not drive AWS itself.
//
// ConfigNamespace/ConfigName point at a plain Secret carrying this
// provider's cluster-level configuration, the place AWS specifics live
// so that ProvisionedNodeClaims do not have to (see
// join.MachineProvisioner). Keys:
//
//	ami-arm64, ami-amd64        per-arch AMI IDs (at least the arch
//	                            in use is required)
//	ssh-key-name                optional EC2 keypair name
//	security-group-ids          optional comma-separated sg-...
//	subnet-id                   optional (CAPA picks from the
//	                            AWSCluster otherwise)
//	iam-instance-profile        optional
//	public-ip                   "true"/"false" (default true: these
//	                            nodes are internet-facing)
//	insecure-skip-secrets-manager "true"/"false" (default true:
//	                            userdata carries only a short-TTL join
//	                            token; SSM adds an IAM dependency for
//	                            no secret kept)
type Provider struct {
	ConfigNamespace    string
	ConfigName         string
	Reader             client.Reader
	SecurityGroupIDs   []string
	SecurityGroupName  string
	IAMInstanceProfile string
	SSHKeyName         string
	// EC2 is an EC2 API in a region, for checks before a machine exists.
	EC2 func(ctx context.Context, region string) (attachmentaws.API, error)
	// WindowsIPAM is whether the cluster runs Windows IPAM, which Windows
	// VPC CNI machines need.
	WindowsIPAM bool
}

var (
	gvk        = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSMachine"}
	clusterGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}

	awsClusterGVK               = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSCluster"}
	awsClusterStaticIdentityGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSClusterStaticIdentity"}
	awsClusterRoleIdentityGVK   = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSClusterRoleIdentity"}
)

// GVK implements join.InfraProvider.
// VPCCNI is the AWS VPC CNI's node class network name.
var VPCCNI = platformaws.VPCCNI{}.Name()

// DefaultCNI makes EC2 machines run the AWS VPC CNI unless their template
// chooses the site CNI.
func (Provider) DefaultCNI(string) string { return VPCCNI }

// NativeNetworks is the pod network EC2 machines can run natively.
func (p Provider) NativeNetworks() []platform.Network {
	return []platform.Network{platformaws.VPCCNI{Windows: p.WindowsIPAM}}
}

func (Provider) GVK() schema.GroupVersionKind { return gvk }

// ClusterGVK implements join.MachineProvisioner.
func (Provider) ClusterGVK() schema.GroupVersionKind { return awsClusterGVK }

// ConfigDefaults merges provider-config Secret values (if Reader and ConfigName
// are configured) with the Provider's explicit flags.
func (p Provider) ConfigDefaults(ctx context.Context) AWSSpecDefaults {
	out := AWSSpecDefaults{
		SecurityGroupIDs:   append([]string(nil), p.SecurityGroupIDs...),
		SecurityGroupName:  strings.TrimSpace(p.SecurityGroupName),
		IAMInstanceProfile: strings.TrimSpace(p.IAMInstanceProfile),
		SSHKeyName:         strings.TrimSpace(p.SSHKeyName),
	}
	if p.Reader == nil || strings.TrimSpace(p.ConfigNamespace) == "" || strings.TrimSpace(p.ConfigName) == "" {
		return out
	}
	sec := &corev1.Secret{}
	if err := p.Reader.Get(ctx, client.ObjectKey{Namespace: p.ConfigNamespace, Name: p.ConfigName}, sec); err != nil {
		return out
	}
	readKey := func(k string) string {
		if v, ok := sec.Data[k]; ok && len(v) > 0 {
			return strings.TrimSpace(string(v))
		}
		if v, ok := sec.StringData[k]; ok && v != "" {
			return strings.TrimSpace(v)
		}
		return ""
	}
	for _, rawID := range strings.Split(readKey("security-group-ids"), ",") {
		if id := strings.TrimSpace(rawID); id != "" {
			out.SecurityGroupIDs = append(out.SecurityGroupIDs, id)
		}
	}
	if out.IAMInstanceProfile == "" {
		out.IAMInstanceProfile = readKey("iam-instance-profile")
	}
	if out.SSHKeyName == "" {
		out.SSHKeyName = readKey("ssh-key-name")
	}
	out.VPCID = readKey("vpc-id")
	out.SubnetID = readKey("subnet-id")
	out.AMIAMD64 = readKey("ami-amd64")
	out.AMIARM64 = readKey("ami-arm64")
	return out
}

// PrepareInfraMachine implements join.InfraMachinePreparer for AWSMachine.
func (p Provider) PrepareInfraMachine(awsMachine *unstructured.Unstructured) (string, error) {
	if awsMachine == nil {
		return "", nil
	}
	var failureDomain string
	if az, found, _ := unstructured.NestedString(awsMachine.Object, "spec", "availabilityZone"); found && az != "" {
		failureDomain = az
		unstructured.RemoveNestedField(awsMachine.Object, "spec", "availabilityZone")
	}
	if fd, found, _ := unstructured.NestedString(awsMachine.Object, "spec", "failureDomain"); found && fd != "" {
		if failureDomain == "" {
			failureDomain = fd
		}
		unstructured.RemoveNestedField(awsMachine.Object, "spec", "failureDomain")
	}
	spec, _ := awsMachine.Object["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		awsMachine.Object["spec"] = spec
	}
	arch := awsMachine.GetLabels()["cloud-provisioning.appmana.com/arch"]
	if err := PrepareAWSMachineSpec(spec, arch, p.ConfigDefaults(context.Background())); err != nil {
		return "", err
	}
	return failureDomain, nil
}

// PrepareAWSMachineSpec populates safe defaults on an AWSMachine spec (or
// AWSMachineTemplate.spec.template.spec) when fields are omitted:
//   - publicIP: true (on-premises WireGuard dialers behind NAT initiate outbound
//     UDP 51820 to the cloud worker's public IP)
//   - iamInstanceProfile: nodes.cluster-api-provider-aws.sigs.k8s.io
//   - cloudInit.insecureSkipSecretsManager: true
//   - additionalSecurityGroups: appends configured WireGuard security group IDs/filters
//   - Official Canonical Ubuntu 24.04 LTS AMI lookup (or aws-provider-config AMI ID)
//     when neither ami.id nor imageLookupBaseOS is specified
func PrepareAWSMachineSpec(spec map[string]any, archLabel string, defaults AWSSpecDefaults) error {
	if spec == nil {
		return nil
	}
	if _, found := spec["publicIP"]; !found {
		spec["publicIP"] = true
	}
	if profile, _ := spec["iamInstanceProfile"].(string); strings.TrimSpace(profile) == "" {
		if defaults.IAMInstanceProfile != "" {
			spec["iamInstanceProfile"] = defaults.IAMInstanceProfile
		} else {
			spec["iamInstanceProfile"] = DefaultIAMInstanceProfile
		}
	}
	cloudInit, _ := spec["cloudInit"].(map[string]any)
	if cloudInit == nil {
		cloudInit = map[string]any{}
		spec["cloudInit"] = cloudInit
	}
	if _, found := cloudInit["insecureSkipSecretsManager"]; !found {
		cloudInit["insecureSkipSecretsManager"] = true
	}

	// Patch in configured security group IDs / filters for on-premises WireGuard connections.
	existingSGs, _ := spec["additionalSecurityGroups"].([]any)
	hasID := func(target string) bool {
		for _, item := range existingSGs {
			if m, ok := item.(map[string]any); ok {
				if id, _ := m["id"].(string); id == target {
					return true
				}
			}
		}
		return false
	}
	for _, rawID := range defaults.SecurityGroupIDs {
		id := strings.TrimSpace(rawID)
		if id != "" && !hasID(id) {
			existingSGs = append(existingSGs, map[string]any{"id": id})
		}
	}
	if sgName := strings.TrimSpace(defaults.SecurityGroupName); sgName != "" {
		hasFilter := false
		for _, item := range existingSGs {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			filters, _ := m["filters"].([]any)
			for _, f := range filters {
				fm, ok := f.(map[string]any)
				if !ok {
					continue
				}
				vals, _ := fm["values"].([]any)
				for _, v := range vals {
					if vs, _ := v.(string); vs == sgName {
						hasFilter = true
					}
				}
			}
		}
		if !hasFilter {
			existingSGs = append(existingSGs, map[string]any{
				"filters": []any{
					map[string]any{
						"name":   "group-name",
						"values": []any{sgName},
					},
				},
			})
		}
	}
	if len(existingSGs) > 0 {
		spec["additionalSecurityGroups"] = existingSGs
	}

	// Default AMI lookup or configured AMI when neither ami.id nor imageLookupBaseOS is set.
	amiMap, _ := spec["ami"].(map[string]any)
	amiID, _ := amiMap["id"].(string)
	lookupBaseOS, _ := spec["imageLookupBaseOS"].(string)
	lookupFormat, _ := spec["imageLookupFormat"].(string)
	if strings.TrimSpace(amiID) == "" && strings.TrimSpace(lookupBaseOS) == "" && strings.TrimSpace(lookupFormat) == "" {
		instType, _ := spec["instanceType"].(string)
		isARM := archLabel == "arm64" || IsARM64InstanceType(instType)
		if isARM && defaults.AMIARM64 != "" {
			spec["ami"] = map[string]any{"id": defaults.AMIARM64}
		} else if !isARM && defaults.AMIAMD64 != "" {
			spec["ami"] = map[string]any{"id": defaults.AMIAMD64}
		} else {
			delete(spec, "ami")
			spec["imageLookupOrg"] = DefaultImageLookupOrg
			spec["imageLookupBaseOS"] = DefaultImageLookupBaseOS
			if isARM {
				spec["imageLookupFormat"] = DefaultImageLookupFormatARM64
			} else {
				spec["imageLookupFormat"] = DefaultImageLookupFormatAMD64
			}
		}
	}

	if _, hasSubnet := spec["subnet"]; !hasSubnet && defaults.SubnetID != "" {
		spec["subnet"] = map[string]any{"id": defaults.SubnetID}
	}
	if sshKey, _ := spec["sshKeyName"].(string); strings.TrimSpace(sshKey) == "" && defaults.SSHKeyName != "" {
		spec["sshKeyName"] = defaults.SSHKeyName
	}
	return nil
}

// IsARM64InstanceType reports whether an EC2 instance type uses an AWS Graviton
// (ARM64) processor family (e.g. t4g.medium, m7g.large, c7gn.xlarge, r6g.large, a1.medium).
func IsARM64InstanceType(instanceType string) bool {
	family := strings.SplitN(strings.ToLower(strings.TrimSpace(instanceType)), ".", 2)[0]
	if family == "a1" {
		return true
	}
	// Graviton families have 'g' after the generation digit (t4g, m6g, m6gd, m7g, c6gn, r7gd, g5g, im4gn, is4gen, x2gd, i4g).
	for i := 1; i < len(family); i++ {
		if family[i-1] >= '0' && family[i-1] <= '9' && family[i] == 'g' {
			return true
		}
	}
	return false
}

// InfraValues supplies platform identity discovery for first boot. CAPA cannot
// allocate an instance ID until it has consumed the rendered bootstrap Secret.
func (Provider) InfraValues(ctx context.Context, awsMachine *unstructured.Unstructured) (map[string]any, error) {
	return bootstrapIdentityValues(), nil
}

// InfraMachine implements join.MachineProvisioner: renders the
// AWSMachine spec for one claim from the provider-config Secret.
// managerNamespace is where CAPA resolves an
// AWSClusterStaticIdentity's secretRef, regardless of where the
// AWSCluster/AWSMachine themselves live (cluster-api-provider-aws
// v2.12.1, pkg/cloud/scope/session.go's buildAWSClusterStaticIdentity
// calls system.GetManagerNamespace(), which reads the CAPA pod's own
// in-cluster namespace file/POD_NAMESPACE, defaulting to
// "capa-system"). It is hardcoded to match this installation rather
// than discovered dynamically: there is one CAPA install per cluster,
// in a namespace fixed by its manifest.
const managerNamespace = "capa-system"

const clusterNameLabel = "cluster.x-k8s.io/cluster-name"

// Validate implements join.Validator: traces an AWSMachine up to its
// Cluster, then its AWSCluster, then its identityRef, and when that identity is an
// AWSClusterStaticIdentity, confirms its secretRef Secret exists in
// managerNamespace. A misplaced Secret produces an indefinite, opaque
// CAPA retry loop ("Secret ... not found") with no signal about where
// it needs to be; this turns that into an immediate, clear error.
//
// Every traversal step before the final Secret check fails open (nil,
// not an error): a missing Cluster/AWSCluster, an unset or
// non-static-identity identityRef, or an unresolvable
// AWSClusterStaticIdentity are either "nothing to validate yet" or out
// of this check's scope. Only the misconfiguration this check exists
// for is surfaced.
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
	// A role identity is assumed by CAPA from its source identity, and that
	// source holds the credentials checked below.
	if identityRefKind == "AWSClusterRoleIdentity" && identityRefName != "" {
		role := &unstructured.Unstructured{}
		role.SetGroupVersionKind(awsClusterRoleIdentityGVK)
		if err := c.Get(ctx, client.ObjectKey{Name: identityRefName}, role); err != nil {
			return nil
		}
		identityRefKind, _, _ = unstructured.NestedString(role.Object, "spec", "sourceIdentityRef", "kind")
		identityRefName, _, _ = unstructured.NestedString(role.Object, "spec", "sourceIdentityRef", "name")
		if identityRefKind == "AWSClusterStaticIdentity" && identityRefName != "" {
			source := &unstructured.Unstructured{}
			source.SetGroupVersionKind(awsClusterStaticIdentityGVK)
			if err := c.Get(ctx, client.ObjectKey{Name: identityRefName}, source); err != nil {
				return fmt.Errorf("AWSClusterRoleIdentity %q assumes its role from AWSClusterStaticIdentity %q, which does not exist: %w", role.GetName(), identityRefName, err)
			}
		}
	}
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
			"AWSClusterStaticIdentity %q references secretRef %q, but CAPA always resolves it against its own manager namespace %q, not %q (where the AWSCluster lives): create/move the Secret to namespace %q: %w",
			identityRefName, secretRefName, managerNamespace, awsCluster.GetNamespace(), managerNamespace, err,
		)
	}
	// An STS session stops working when it expires, and nothing renews a
	// Secret: CAPA would fail every call from then on. CAPA renews a role
	// itself when an AWSClusterRoleIdentity assumes it.
	if len(secret.Data["SessionToken"]) > 0 {
		return fmt.Errorf("AWSClusterStaticIdentity %q holds an expiring STS session (SessionToken), which nothing renews; give it long-lived keys that may only assume CAPA's role, and point the AWSCluster at an AWSClusterRoleIdentity with that role and this identity as its sourceIdentityRef", identityRefName)
	}
	return nil
}
