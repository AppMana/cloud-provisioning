// Package iam renders, audits, and reconciles the least-privilege AWS IAM
// policies, roles, and instance profiles required for CAPI/CAPA Linux and
// Windows provisioning, AWS VPC CNI IPAM, AMI lifecycle & boot performance
// (Windows Fast Launch, EBS volume sizing, NVIDIA GRID), and observability.
package iam

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed policies/*.json
var policyFS embed.FS

// ManagedPolicyLimitBytes is AWS IAM's maximum character limit for a Customer
// Managed Policy document (excluding whitespace).
const ManagedPolicyLimitBytes = 6144

// SSMManagedInstanceCoreARN is the AWS-managed policy granting Systems Manager
// core agent permissions on EC2 workers.
const SSMManagedInstanceCoreARN = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"

// PolicyID identifies a canonical policy template in policies/*.json.
type PolicyID string

const (
	PolicyCAPAProvisioning      PolicyID = "capa-provisioning"
	PolicyWorkerBootstrap       PolicyID = "worker-bootstrap"
	PolicyVPCCNINodeIPv4        PolicyID = "vpc-cni-node-ipv4"
	PolicyVPCCNINodeIPv6        PolicyID = "vpc-cni-node-ipv6"
	PolicyWorkerObservability   PolicyID = "worker-observability"
	PolicyController            PolicyID = "controller"
	PolicyControllerArtifacts   PolicyID = "controller-artifacts"
	PolicyAMILifecycle          PolicyID = "ami-lifecycle"
	PolicyAMIPerformance        PolicyID = "ami-performance"
	PolicyOperatorObservability PolicyID = "operator-observability"
)

// Options configures tuple-aware IAM policy and role rendering.
type Options struct {
	AccountID          string   `json:"accountID"`
	Region             string   `json:"region"`
	ClusterName        string   `json:"clusterName"`
	RolePrefix         string   `json:"rolePrefix,omitempty"`
	OwnerTag           string   `json:"ownerTag,omitempty"`
	OwnerValue         string   `json:"ownerValue,omitempty"`
	SourcePrincipalARN string   `json:"sourcePrincipalARN,omitempty"`
	AMIs               []string `json:"amis,omitempty"`
	Subnets            []string `json:"subnets,omitempty"`
	SecurityGroups     []string `json:"securityGroups,omitempty"`
	AssetBucket        string   `json:"assetBucket,omitempty"`
	AssetPrefix        string   `json:"assetPrefix,omitempty"`

	// Tuple-driven capabilities
	IncludeWindows        bool   `json:"includeWindows"`
	IncludeVPCCNI         bool   `json:"includeVPCCNI"`
	VPCCNIFamily          string `json:"vpcCNIFamily,omitempty"` // "ipv4", "ipv6", or "dual"
	IncludeAMILifecycle   bool   `json:"includeAMILifecycle"`
	IncludeAMIPerformance bool   `json:"includeAMIPerformance"`
	IncludeObservability  bool   `json:"includeObservability"`

	// Explicit role/profile name overrides
	CAPARoleName        string `json:"capaRoleName,omitempty"`
	ControllerRoleName  string `json:"controllerRoleName,omitempty"`
	WorkerRoleName      string `json:"workerRoleName,omitempty"`
	InstanceProfileName string `json:"instanceProfileName,omitempty"`
	OperatorRoleName    string `json:"operatorRoleName,omitempty"`
}

// ManagedPolicy is a rendered Customer Managed Policy ready to create/update in AWS IAM.
type ManagedPolicy struct {
	ID          PolicyID `json:"id"`
	Name        string   `json:"name"`
	ARN         string   `json:"arn"`
	Description string   `json:"description"`
	Document    string   `json:"document"`
	CompactSize int      `json:"compactSize"`
	SHA256      string   `json:"sha256"`
}

// RoleSpec describes an IAM Role and the managed policies attached to it.
type RoleSpec struct {
	Name                string   `json:"name"`
	ARN                 string   `json:"arn"`
	Description         string   `json:"description"`
	AssumeRolePolicyDoc string   `json:"assumeRolePolicyDocument"`
	ManagedPolicyNames  []string `json:"managedPolicyNames"`
	ManagedPolicyARNs   []string `json:"managedPolicyARNs"`
}

// InstanceProfileSpec describes the EC2 Instance Profile bound to the worker role.
type InstanceProfileSpec struct {
	Name     string `json:"name"`
	ARN      string `json:"arn"`
	RoleName string `json:"roleName"`
}

// Bundle is the complete rendered IAM state for a cluster and platform tuple.
type Bundle struct {
	AccountID       string              `json:"accountID"`
	Region          string              `json:"region"`
	ClusterName     string              `json:"clusterName"`
	Policies        []ManagedPolicy     `json:"policies"`
	Roles           []RoleSpec          `json:"roles"`
	InstanceProfile InstanceProfileSpec `json:"instanceProfile"`
}

var templateVarPattern = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

// RawPolicyTemplate returns the embedded raw JSON template for a policy file name.
func RawPolicyTemplate(filename string) ([]byte, error) {
	if !strings.HasSuffix(filename, ".json") {
		filename += ".json"
	}
	return policyFS.ReadFile("policies/" + filename)
}

// Normalize fills default role names, tags, and tuple defaults on Options.
func (o *Options) Normalize() error {
	o.AccountID = strings.TrimSpace(o.AccountID)
	o.Region = strings.TrimSpace(o.Region)
	o.ClusterName = strings.TrimSpace(o.ClusterName)
	if o.AccountID == "" {
		return fmt.Errorf("AWS accountID is required")
	}
	if o.Region == "" {
		return fmt.Errorf("AWS region is required")
	}
	if o.ClusterName == "" {
		return fmt.Errorf("clusterName is required")
	}
	prefix := strings.TrimSpace(o.RolePrefix)
	if prefix == "" {
		prefix = o.ClusterName
	}
	o.RolePrefix = prefix
	if o.OwnerTag == "" {
		o.OwnerTag = "sigs.k8s.io/cluster-api-provider-aws/cluster/" + o.ClusterName
	}
	if o.OwnerValue == "" {
		o.OwnerValue = "owned"
	}
	if o.SourcePrincipalARN == "" {
		o.SourcePrincipalARN = fmt.Sprintf("arn:aws:iam::%s:root", o.AccountID)
	}
	if o.CAPARoleName == "" {
		o.CAPARoleName = prefix + "-capa-controller"
	}
	if o.ControllerRoleName == "" {
		o.ControllerRoleName = prefix + "-endpoint-controller"
	}
	if o.WorkerRoleName == "" {
		o.WorkerRoleName = prefix + "-worker-node"
	}
	if o.InstanceProfileName == "" {
		o.InstanceProfileName = o.WorkerRoleName
	}
	if o.OperatorRoleName == "" {
		o.OperatorRoleName = prefix + "-ami-operator"
	}
	if o.VPCCNIFamily == "" {
		o.VPCCNIFamily = "ipv4"
	}
	return nil
}

// Render builds the complete IAM Bundle for the supplied Options.
func Render(opts Options) (*Bundle, error) {
	if err := opts.Normalize(); err != nil {
		return nil, err
	}

	amiID := "*"
	if len(opts.AMIs) == 1 {
		amiID = opts.AMIs[0]
	}
	subnetID := "*"
	if len(opts.Subnets) == 1 {
		subnetID = opts.Subnets[0]
	}
	sgID := "*"
	if len(opts.SecurityGroups) == 1 {
		sgID = opts.SecurityGroups[0]
	}

	vars := map[string]string{
		"AWS_ACCOUNT_ID":        opts.AccountID,
		"AWS_REGION":            opts.Region,
		"CLUSTER_NAME":          opts.ClusterName,
		"OWNER_TAG":             opts.OwnerTag,
		"OWNER_VALUE":           opts.OwnerValue,
		"AMI_ID":                amiID,
		"SUBNET_ID":             subnetID,
		"SECURITY_GROUP_ID":     sgID,
		"NODE_ROLE_NAME":        opts.WorkerRoleName,
		"INSTANCE_PROFILE_NAME": opts.InstanceProfileName,
		"SOURCE_PRINCIPAL_ARN":  opts.SourcePrincipalARN,
		"ASSET_BUCKET":          opts.AssetBucket,
		"ASSET_PREFIX":          opts.AssetPrefix,
	}

	var policies []ManagedPolicy
	addPolicy := func(id PolicyID, desc string, mutate func(doc map[string]any)) error {
		mp, err := renderManagedPolicy(opts, id, desc, vars, mutate)
		if err != nil {
			return err
		}
		policies = append(policies, mp)
		return nil
	}

	// 1. CAPA Provisioning Policy (supports multi-AMI, multi-subnet, multi-SG arrays)
	if err := addPolicy(PolicyCAPAProvisioning,
		"CAPI/CAPA EC2 and Secrets Manager lifecycle for Linux and Windows workers",
		func(doc map[string]any) {
			expandLaunchResourceARNs(doc, opts)
		}); err != nil {
		return nil, err
	}

	// 2. Worker Bootstrap Policy (Linux cloud-init & Windows EC2Launch v2 Secrets Manager)
	if err := addPolicy(PolicyWorkerBootstrap,
		"Worker bootstrap secret consumption for Linux cloud-init and Windows EC2Launch v2",
		nil); err != nil {
		return nil, err
	}

	// 3. Worker & Controller VPC CNI IPAM Policies (when native aws-vpc CNI is enabled)
	var vpcCNIPolicyNames []string
	var vpcCNIPolicyARNs []string
	if opts.IncludeVPCCNI {
		fam := strings.ToLower(strings.TrimSpace(opts.VPCCNIFamily))
		if fam == "ipv4" || fam == "dual" || fam == "" {
			if err := addPolicy(PolicyVPCCNINodeIPv4,
				"AWS VPC CNI IPv4 secondary address allocation on cluster-owned ENIs",
				nil); err != nil {
				return nil, err
			}
			p := policies[len(policies)-1]
			vpcCNIPolicyNames = append(vpcCNIPolicyNames, p.Name)
			vpcCNIPolicyARNs = append(vpcCNIPolicyARNs, p.ARN)
		}
		if fam == "ipv6" || fam == "dual" {
			if err := addPolicy(PolicyVPCCNINodeIPv6,
				"AWS VPC CNI IPv6 /80 prefix delegation on cluster-owned ENIs",
				nil); err != nil {
				return nil, err
			}
			p := policies[len(policies)-1]
			vpcCNIPolicyNames = append(vpcCNIPolicyNames, p.Name)
			vpcCNIPolicyARNs = append(vpcCNIPolicyARNs, p.ARN)
		}
	}

	// 4. Endpoint Controller Runtime Policy
	if err := addPolicy(PolicyController,
		"Endpoint controller VPC CNI allocation observer and security-group validator",
		nil); err != nil {
		return nil, err
	}
	var controllerArtifactsPolicy *ManagedPolicy
	if opts.AssetBucket != "" {
		if err := addPolicy(PolicyControllerArtifacts,
			"Endpoint controller S3 bootstrap artifact presigner",
			nil); err != nil {
			return nil, err
		}
		p := policies[len(policies)-1]
		controllerArtifactsPolicy = &p
	}

	// 5. Observability Policies (Worker telemetry + Operator console/SSM diagnostics)
	var workerObsPolicy *ManagedPolicy
	var operatorObsPolicy *ManagedPolicy
	if opts.IncludeObservability {
		if err := addPolicy(PolicyWorkerObservability,
			"Worker CloudWatch logs/metrics and instance tag self-inspection",
			nil); err != nil {
			return nil, err
		}
		p1 := policies[len(policies)-1]
		workerObsPolicy = &p1

		if err := addPolicy(PolicyOperatorObservability,
			"Operator EC2 console output/screenshots, SSM diagnostics, and CloudWatch log/metric reads",
			nil); err != nil {
			return nil, err
		}
		p2 := policies[len(policies)-1]
		operatorObsPolicy = &p2
	}

	// 6. AMI Lifecycle & Windows/GPU Boot Performance Policies
	var amiLifecyclePolicy *ManagedPolicy
	var amiPerfPolicy *ManagedPolicy
	if opts.IncludeAMILifecycle {
		if err := addPolicy(PolicyAMILifecycle,
			"Linux and Windows AMI capture, registration, EBS volume sizing, and NVIDIA GRID driver retrieval",
			nil); err != nil {
			return nil, err
		}
		p := policies[len(policies)-1]
		amiLifecyclePolicy = &p
	}
	if opts.IncludeAMIPerformance {
		if err := addPolicy(PolicyAMIPerformance,
			"Windows EC2 Fast Launch snapshot pre-provisioning and EBS snapshot warming",
			nil); err != nil {
			return nil, err
		}
		p := policies[len(policies)-1]
		amiPerfPolicy = &p
	}

	// Render Role trust policies
	capaTrustDoc, err := renderTemplateFile("capa-trust.json", vars)
	if err != nil {
		return nil, err
	}
	workerTrustDoc, err := renderTemplateFile("worker-trust.json", vars)
	if err != nil {
		return nil, err
	}

	policyByID := map[PolicyID]ManagedPolicy{}
	for _, p := range policies {
		policyByID[p.ID] = p
	}

	// Role 1: CAPA Controller Role
	capaPolicy := policyByID[PolicyCAPAProvisioning]
	roles := []RoleSpec{
		{
			Name:                opts.CAPARoleName,
			ARN:                 fmt.Sprintf("arn:aws:iam::%s:role/%s", opts.AccountID, opts.CAPARoleName),
			Description:         fmt.Sprintf("Cluster API Provider AWS controller role for %s", opts.ClusterName),
			AssumeRolePolicyDoc: capaTrustDoc,
			ManagedPolicyNames:  []string{capaPolicy.Name},
			ManagedPolicyARNs:   []string{capaPolicy.ARN},
		},
	}

	// Role 2: Endpoint Controller Role
	ctrlPolicy := policyByID[PolicyController]
	ctrlRole := RoleSpec{
		Name:                opts.ControllerRoleName,
		ARN:                 fmt.Sprintf("arn:aws:iam::%s:role/%s", opts.AccountID, opts.ControllerRoleName),
		Description:         fmt.Sprintf("cloud-provisioning endpoint-controller role for %s", opts.ClusterName),
		AssumeRolePolicyDoc: capaTrustDoc,
		ManagedPolicyNames:  []string{ctrlPolicy.Name},
		ManagedPolicyARNs:   []string{ctrlPolicy.ARN},
	}
	if controllerArtifactsPolicy != nil {
		ctrlRole.ManagedPolicyNames = append(ctrlRole.ManagedPolicyNames, controllerArtifactsPolicy.Name)
		ctrlRole.ManagedPolicyARNs = append(ctrlRole.ManagedPolicyARNs, controllerArtifactsPolicy.ARN)
	}
	// When Windows + AWS VPC CNI is used, aws-vpc-resource-controller also needs IPv4 ENI IPAM permissions.
	if opts.IncludeWindows && opts.IncludeVPCCNI {
		if p, ok := policyByID[PolicyVPCCNINodeIPv4]; ok {
			ctrlRole.ManagedPolicyNames = append(ctrlRole.ManagedPolicyNames, p.Name)
			ctrlRole.ManagedPolicyARNs = append(ctrlRole.ManagedPolicyARNs, p.ARN)
		}
	}
	roles = append(roles, ctrlRole)

	// Role 3: EC2 Worker Role (+ Instance Profile)
	wbPolicy := policyByID[PolicyWorkerBootstrap]
	workerRole := RoleSpec{
		Name:                opts.WorkerRoleName,
		ARN:                 fmt.Sprintf("arn:aws:iam::%s:role/%s", opts.AccountID, opts.WorkerRoleName),
		Description:         fmt.Sprintf("EC2 cloud worker node role for %s", opts.ClusterName),
		AssumeRolePolicyDoc: workerTrustDoc,
		ManagedPolicyNames:  []string{wbPolicy.Name},
		ManagedPolicyARNs:   []string{wbPolicy.ARN},
	}
	workerRole.ManagedPolicyNames = append(workerRole.ManagedPolicyNames, vpcCNIPolicyNames...)
	workerRole.ManagedPolicyARNs = append(workerRole.ManagedPolicyARNs, vpcCNIPolicyARNs...)
	if workerObsPolicy != nil {
		workerRole.ManagedPolicyNames = append(workerRole.ManagedPolicyNames, workerObsPolicy.Name)
		workerRole.ManagedPolicyARNs = append(workerRole.ManagedPolicyARNs, workerObsPolicy.ARN, SSMManagedInstanceCoreARN)
	}
	roles = append(roles, workerRole)

	// Role 4: AMI & Observability Operator Role (when AMI lifecycle, performance, or observability is requested)
	if amiLifecyclePolicy != nil || amiPerfPolicy != nil || operatorObsPolicy != nil {
		opRole := RoleSpec{
			Name:                opts.OperatorRoleName,
			ARN:                 fmt.Sprintf("arn:aws:iam::%s:role/%s", opts.AccountID, opts.OperatorRoleName),
			Description:         fmt.Sprintf("AMI builder, Fast Launch performance, and diagnostics operator role for %s", opts.ClusterName),
			AssumeRolePolicyDoc: capaTrustDoc,
		}
		if amiLifecyclePolicy != nil {
			opRole.ManagedPolicyNames = append(opRole.ManagedPolicyNames, amiLifecyclePolicy.Name)
			opRole.ManagedPolicyARNs = append(opRole.ManagedPolicyARNs, amiLifecyclePolicy.ARN)
		}
		if amiPerfPolicy != nil {
			opRole.ManagedPolicyNames = append(opRole.ManagedPolicyNames, amiPerfPolicy.Name)
			opRole.ManagedPolicyARNs = append(opRole.ManagedPolicyARNs, amiPerfPolicy.ARN)
		}
		if operatorObsPolicy != nil {
			opRole.ManagedPolicyNames = append(opRole.ManagedPolicyNames, operatorObsPolicy.Name)
			opRole.ManagedPolicyARNs = append(opRole.ManagedPolicyARNs, operatorObsPolicy.ARN)
		}
		roles = append(roles, opRole)
	}

	return &Bundle{
		AccountID:   opts.AccountID,
		Region:      opts.Region,
		ClusterName: opts.ClusterName,
		Policies:    policies,
		Roles:       roles,
		InstanceProfile: InstanceProfileSpec{
			Name:     opts.InstanceProfileName,
			ARN:      fmt.Sprintf("arn:aws:iam::%s:instance-profile/%s", opts.AccountID, opts.InstanceProfileName),
			RoleName: opts.WorkerRoleName,
		},
	}, nil
}

func renderManagedPolicy(opts Options, id PolicyID, description string, vars map[string]string, mutate func(map[string]any)) (ManagedPolicy, error) {
	rawDoc, err := renderTemplateFile(string(id)+".json", vars)
	if err != nil {
		return ManagedPolicy{}, err
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(rawDoc), &parsed); err != nil {
		return ManagedPolicy{}, fmt.Errorf("parsing rendered policy %s: %w", id, err)
	}
	if mutate != nil {
		mutate(parsed)
	}
	formatted, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return ManagedPolicy{}, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, formatted); err != nil {
		return ManagedPolicy{}, err
	}
	if compact.Len() > ManagedPolicyLimitBytes {
		return ManagedPolicy{}, fmt.Errorf("policy %s compact size %d exceeds AWS 6144-byte managed policy limit", id, compact.Len())
	}
	sum := sha256.Sum256(formatted)
	name := fmt.Sprintf("%s-%s", opts.RolePrefix, id)
	return ManagedPolicy{
		ID:          id,
		Name:        name,
		ARN:         fmt.Sprintf("arn:aws:iam::%s:policy/%s", opts.AccountID, name),
		Description: description,
		Document:    string(formatted),
		CompactSize: compact.Len(),
		SHA256:      hex.EncodeToString(sum[:]),
	}, nil
}

func renderTemplateFile(filename string, vars map[string]string) (string, error) {
	raw, err := RawPolicyTemplate(filename)
	if err != nil {
		return "", err
	}
	var missing []string
	out := templateVarPattern.ReplaceAllStringFunc(string(raw), func(m string) string {
		sub := templateVarPattern.FindStringSubmatch(m)
		key := sub[1]
		val, ok := vars[key]
		if !ok {
			missing = append(missing, key)
			return m
		}
		escaped, _ := json.Marshal(val)
		return string(escaped[1 : len(escaped)-1])
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("template %s missing variables: %v", filename, missing)
	}
	return out, nil
}

// expandLaunchResourceARNs replaces single-value AMI/Subnet/SecurityGroup ARNs in
// UseApprovedLaunchResources with the full list of configured AMIs, Subnets, and SGs.
func expandLaunchResourceARNs(doc map[string]any, opts Options) {
	stmts, ok := doc["Statement"].([]any)
	if !ok {
		return
	}
	for _, rawStmt := range stmts {
		stmt, ok := rawStmt.(map[string]any)
		if !ok || stmt["Sid"] != "UseApprovedLaunchResources" {
			continue
		}
		var resources []string
		if len(opts.AMIs) == 0 {
			resources = append(resources, fmt.Sprintf("arn:aws:ec2:%s::image/*", opts.Region))
		} else {
			for _, ami := range opts.AMIs {
				if strings.TrimSpace(ami) != "" {
					resources = append(resources, fmt.Sprintf("arn:aws:ec2:%s::image/%s", opts.Region, strings.TrimSpace(ami)))
				}
			}
		}
		if len(opts.Subnets) == 0 {
			resources = append(resources, fmt.Sprintf("arn:aws:ec2:%s:%s:subnet/*", opts.Region, opts.AccountID))
		} else {
			for _, sub := range opts.Subnets {
				if strings.TrimSpace(sub) != "" {
					resources = append(resources, fmt.Sprintf("arn:aws:ec2:%s:%s:subnet/%s", opts.Region, opts.AccountID, strings.TrimSpace(sub)))
				}
			}
		}
		if len(opts.SecurityGroups) == 0 {
			resources = append(resources, fmt.Sprintf("arn:aws:ec2:%s:%s:security-group/*", opts.Region, opts.AccountID))
		} else {
			for _, sg := range opts.SecurityGroups {
				if strings.TrimSpace(sg) != "" {
					resources = append(resources, fmt.Sprintf("arn:aws:ec2:%s:%s:security-group/%s", opts.Region, opts.AccountID, strings.TrimSpace(sg)))
				}
			}
		}
		stmt["Resource"] = resources
	}
}

// DiscoverOptionsFromCluster inspects live Kubernetes AWSCluster, AWSMachineTemplate,
// and AWSClusterRoleIdentity objects to pre-populate Region, ClusterName, Subnets,
// SecurityGroups, AMIs, InstanceProfileName, and CAPARoleName.
func DiscoverOptionsFromCluster(ctx context.Context, reader client.Reader, opts *Options) error {
	if reader == nil || opts == nil {
		return nil
	}
	awsClusters := &unstructured.UnstructuredList{}
	awsClusters.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta2",
		Kind:    "AWSClusterList",
	})
	if err := reader.List(ctx, awsClusters); err != nil && !meta.IsNoMatchError(err) {
		return err
	}
	for _, item := range awsClusters.Items {
		if opts.ClusterName == "" {
			opts.ClusterName = item.GetName()
		}
		if region, _, _ := unstructured.NestedString(item.Object, "spec", "region"); region != "" && opts.Region == "" {
			opts.Region = region
		}
		if specs, _, _ := unstructured.NestedSlice(item.Object, "spec", "network", "subnets"); len(specs) > 0 && len(opts.Subnets) == 0 {
			for _, raw := range specs {
				if m, ok := raw.(map[string]any); ok {
					id, _ := m["resourceID"].(string)
					if id == "" {
						id, _ = m["id"].(string)
					}
					if id != "" && !slices.Contains(opts.Subnets, id) {
						opts.Subnets = append(opts.Subnets, id)
					}
				}
			}
		}
	}

	templates := &unstructured.UnstructuredList{}
	templates.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta2",
		Kind:    "AWSMachineTemplateList",
	})
	if err := reader.List(ctx, templates); err == nil {
		for _, item := range templates.Items {
			spec, _, _ := unstructured.NestedMap(item.Object, "spec", "template", "spec")
			if spec == nil {
				continue
			}
			if ami, _, _ := unstructured.NestedString(spec, "ami", "id"); ami != "" && !slices.Contains(opts.AMIs, ami) {
				opts.AMIs = append(opts.AMIs, ami)
			}
			if profile, _, _ := unstructured.NestedString(spec, "iamInstanceProfile"); profile != "" && opts.InstanceProfileName == "" {
				opts.InstanceProfileName = profile
				if opts.WorkerRoleName == "" {
					opts.WorkerRoleName = profile
				}
			}
			if sgs, _, _ := unstructured.NestedSlice(spec, "additionalSecurityGroups"); len(sgs) > 0 {
				for _, raw := range sgs {
					if m, ok := raw.(map[string]any); ok {
						if id, _ := m["id"].(string); id != "" && !slices.Contains(opts.SecurityGroups, id) {
							opts.SecurityGroups = append(opts.SecurityGroups, id)
						}
					}
				}
			}
		}
	}
	return nil
}
