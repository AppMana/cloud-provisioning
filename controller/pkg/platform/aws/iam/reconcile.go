package iam

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PolicyVersion describes an existing version of an AWS IAM Customer Managed Policy.
type PolicyVersion struct {
	VersionID        string `json:"versionID"`
	IsDefaultVersion bool   `json:"isDefaultVersion"`
	CreateDate       string `json:"createDate,omitempty"`
}

// InstanceProfile describes an existing AWS IAM Instance Profile and its bound roles.
type InstanceProfile struct {
	Name      string   `json:"name"`
	ARN       string   `json:"arn"`
	RoleNames []string `json:"roleNames"`
}

// API abstracts the AWS IAM operations needed at installation time to create,
// update, and verify Customer Managed Policies, Roles, and Instance Profiles.
type API interface {
	GetPolicy(ctx context.Context, arn string) (exists bool, defaultVersionID string, err error)
	GetPolicyVersionDocument(ctx context.Context, arn, versionID string) (string, error)
	CreatePolicy(ctx context.Context, name, description, document string) (arn string, err error)
	ListPolicyVersions(ctx context.Context, arn string) ([]PolicyVersion, error)
	DeletePolicyVersion(ctx context.Context, arn, versionID string) error
	CreatePolicyVersion(ctx context.Context, arn, document string, setAsDefault bool) (versionID string, err error)

	GetRole(ctx context.Context, name string) (exists bool, assumeRoleDoc string, err error)
	CreateRole(ctx context.Context, name, description, assumeRoleDoc string) (arn string, err error)
	UpdateAssumeRolePolicy(ctx context.Context, name, assumeRoleDoc string) error
	ListAttachedRolePolicies(ctx context.Context, roleName string) (policyARNs []string, err error)
	AttachRolePolicy(ctx context.Context, roleName, policyARN string) error

	GetInstanceProfile(ctx context.Context, name string) (*InstanceProfile, error)
	CreateInstanceProfile(ctx context.Context, name string) (arn string, err error)
	AddRoleToInstanceProfile(ctx context.Context, profileName, roleName string) error
}

// ReconcileReport summarizes the IAM policies, roles, instance profile, and
// Kubernetes identity resources created or updated during installation.
type ReconcileReport struct {
	DryRun                  bool     `json:"dryRun,omitempty"`
	AccountID               string   `json:"accountID"`
	Region                  string   `json:"region"`
	ClusterName             string   `json:"clusterName"`
	PoliciesCreated         []string `json:"policiesCreated,omitempty"`
	PoliciesUpdated         []string `json:"policiesUpdated,omitempty"`
	RolesCreated            []string `json:"rolesCreated,omitempty"`
	RolesUpdated            []string `json:"rolesUpdated,omitempty"`
	InstanceProfileEnsured  string   `json:"instanceProfileEnsured,omitempty"`
	ClusterIdentityUpdated  string   `json:"clusterIdentityUpdated,omitempty"`
	Changes                 []string `json:"changes,omitempty"`
}

// Reconcile idempotently creates or updates all Customer Managed Policies, IAM Roles,
// policy attachments, and the EC2 Instance Profile in the Bundle.
func Reconcile(ctx context.Context, api API, bundle *Bundle, dryRun bool) (*ReconcileReport, error) {
	if bundle == nil {
		return nil, fmt.Errorf("IAM bundle is required")
	}
	if api == nil && !dryRun {
		return nil, fmt.Errorf("IAM API client is required when dryRun is false")
	}
	report := &ReconcileReport{
		DryRun:      dryRun,
		AccountID:   bundle.AccountID,
		Region:      bundle.Region,
		ClusterName: bundle.ClusterName,
	}

	// 1. Reconcile Customer Managed Policies
	for _, pol := range bundle.Policies {
		if api == nil {
			report.PoliciesCreated = append(report.PoliciesCreated, pol.ARN)
			report.Changes = append(report.Changes, fmt.Sprintf("would ensure customer managed policy %s (%d bytes)", pol.ARN, pol.CompactSize))
			continue
		}
		exists, defaultVer, err := api.GetPolicy(ctx, pol.ARN)
		if err != nil {
			return report, fmt.Errorf("checking IAM policy %s: %w", pol.ARN, err)
		}
		if !exists {
			if !dryRun {
				if _, err := api.CreatePolicy(ctx, pol.Name, pol.Description, pol.Document); err != nil {
					return report, fmt.Errorf("creating IAM policy %s: %w", pol.Name, err)
				}
			}
			report.PoliciesCreated = append(report.PoliciesCreated, pol.ARN)
			report.Changes = append(report.Changes, fmt.Sprintf("created customer managed policy %s (%d bytes)", pol.ARN, pol.CompactSize))
			continue
		}
		currentDoc, err := api.GetPolicyVersionDocument(ctx, pol.ARN, defaultVer)
		if err == nil && normalizeJSON(currentDoc) == normalizeJSON(pol.Document) {
			continue
		}
		if !dryRun {
			versions, err := api.ListPolicyVersions(ctx, pol.ARN)
			if err != nil {
				return report, fmt.Errorf("listing versions of IAM policy %s: %w", pol.ARN, err)
			}
			// AWS allows at most 5 policy versions; prune the oldest non-default version first.
			if len(versions) >= 5 {
				oldest := oldestNonDefaultVersion(versions)
				if oldest != "" {
					if err := api.DeletePolicyVersion(ctx, pol.ARN, oldest); err != nil {
						return report, fmt.Errorf("deleting oldest version %s of IAM policy %s: %w", oldest, pol.ARN, err)
					}
				}
			}
			if _, err := api.CreatePolicyVersion(ctx, pol.ARN, pol.Document, true); err != nil {
				return report, fmt.Errorf("updating IAM policy %s: %w", pol.ARN, err)
			}
		}
		report.PoliciesUpdated = append(report.PoliciesUpdated, pol.ARN)
		report.Changes = append(report.Changes, fmt.Sprintf("updated customer managed policy %s to new default version (%d bytes)", pol.ARN, pol.CompactSize))
	}

	// 2. Reconcile IAM Roles and Policy Attachments
	for _, role := range bundle.Roles {
		if api == nil {
			report.RolesCreated = append(report.RolesCreated, role.ARN)
			report.Changes = append(report.Changes, fmt.Sprintf("would ensure IAM role %s with %d attached policies", role.Name, len(role.ManagedPolicyARNs)))
			continue
		}
		exists, currentTrust, err := api.GetRole(ctx, role.Name)
		if err != nil {
			return report, fmt.Errorf("checking IAM role %s: %w", role.Name, err)
		}
		if !exists {
			if !dryRun {
				if _, err := api.CreateRole(ctx, role.Name, role.Description, role.AssumeRolePolicyDoc); err != nil {
					return report, fmt.Errorf("creating IAM role %s: %w", role.Name, err)
				}
			}
			report.RolesCreated = append(report.RolesCreated, role.ARN)
			report.Changes = append(report.Changes, fmt.Sprintf("created IAM role %s", role.ARN))
		} else if normalizeJSON(currentTrust) != normalizeJSON(role.AssumeRolePolicyDoc) {
			if !dryRun {
				if err := api.UpdateAssumeRolePolicy(ctx, role.Name, role.AssumeRolePolicyDoc); err != nil {
					return report, fmt.Errorf("updating trust policy on IAM role %s: %w", role.Name, err)
				}
			}
			report.RolesUpdated = append(report.RolesUpdated, role.ARN)
			report.Changes = append(report.Changes, fmt.Sprintf("updated assume-role trust policy on IAM role %s", role.Name))
		}

		var attached []string
		if exists {
			attached, err = api.ListAttachedRolePolicies(ctx, role.Name)
			if err != nil {
				return report, fmt.Errorf("listing attached policies on IAM role %s: %w", role.Name, err)
			}
		}
		for _, polARN := range role.ManagedPolicyARNs {
			if slices.Contains(attached, polARN) {
				continue
			}
			if !dryRun {
				if err := api.AttachRolePolicy(ctx, role.Name, polARN); err != nil {
					return report, fmt.Errorf("attaching policy %s to IAM role %s: %w", polARN, role.Name, err)
				}
			}
			if !slices.Contains(report.RolesUpdated, role.ARN) && !slices.Contains(report.RolesCreated, role.ARN) {
				report.RolesUpdated = append(report.RolesUpdated, role.ARN)
			}
			report.Changes = append(report.Changes, fmt.Sprintf("attached policy %s to IAM role %s", polARN, role.Name))
		}
	}

	// 3. Reconcile Worker EC2 Instance Profile
	prof := bundle.InstanceProfile
	if prof.Name != "" {
		if api == nil {
			report.InstanceProfileEnsured = prof.ARN
			report.Changes = append(report.Changes, fmt.Sprintf("would ensure instance profile %s bound to role %s", prof.Name, prof.RoleName))
			return report, nil
		}
		existingProf, err := api.GetInstanceProfile(ctx, prof.Name)
		if err != nil {
			return report, fmt.Errorf("checking instance profile %s: %w", prof.Name, err)
		}
		if existingProf == nil {
			if !dryRun {
				if _, err := api.CreateInstanceProfile(ctx, prof.Name); err != nil {
					return report, fmt.Errorf("creating instance profile %s: %w", prof.Name, err)
				}
			}
			report.Changes = append(report.Changes, fmt.Sprintf("created EC2 instance profile %s", prof.ARN))
		}
		if existingProf == nil || !slices.Contains(existingProf.RoleNames, prof.RoleName) {
			if !dryRun {
				if err := api.AddRoleToInstanceProfile(ctx, prof.Name, prof.RoleName); err != nil {
					return report, fmt.Errorf("binding role %s to instance profile %s: %w", prof.RoleName, prof.Name, err)
				}
			}
			report.Changes = append(report.Changes, fmt.Sprintf("bound role %s to EC2 instance profile %s", prof.RoleName, prof.Name))
		}
		report.InstanceProfileEnsured = prof.ARN
	}

	return report, nil
}

// Audit inspects live AWS IAM state against the desired Bundle and returns human-readable
// findings (empty slice if all required policies, roles, and instance profiles are in place).
func Audit(ctx context.Context, api API, bundle *Bundle) ([]string, error) {
	if api == nil || bundle == nil {
		return nil, nil
	}
	var findings []string
	for _, pol := range bundle.Policies {
		exists, _, err := api.GetPolicy(ctx, pol.ARN)
		if err != nil {
			return nil, err
		}
		if !exists {
			findings = append(findings, fmt.Sprintf("missing Customer Managed Policy %s (%s)", pol.Name, pol.Description))
		}
	}
	for _, role := range bundle.Roles {
		exists, _, err := api.GetRole(ctx, role.Name)
		if err != nil {
			return nil, err
		}
		if !exists {
			findings = append(findings, fmt.Sprintf("missing IAM Role %s", role.Name))
			continue
		}
		attached, err := api.ListAttachedRolePolicies(ctx, role.Name)
		if err != nil {
			return nil, err
		}
		for _, wantARN := range role.ManagedPolicyARNs {
			if !slices.Contains(attached, wantARN) {
				findings = append(findings, fmt.Sprintf("IAM Role %s is missing attached policy %s", role.Name, wantARN))
			}
		}
	}
	if bundle.InstanceProfile.Name != "" {
		prof, err := api.GetInstanceProfile(ctx, bundle.InstanceProfile.Name)
		if err != nil {
			return nil, err
		}
		if prof == nil {
			findings = append(findings, fmt.Sprintf("missing EC2 Instance Profile %s", bundle.InstanceProfile.Name))
		} else if !slices.Contains(prof.RoleNames, bundle.InstanceProfile.RoleName) {
			findings = append(findings, fmt.Sprintf("EC2 Instance Profile %s is not bound to role %s", bundle.InstanceProfile.Name, bundle.InstanceProfile.RoleName))
		}
	}
	return findings, nil
}

// ReconcileAWSClusterRoleIdentity ensures an AWSClusterRoleIdentity CR in Kubernetes
// points CAPA at the reconciled CAPA controller IAM role ARN.
func ReconcileAWSClusterRoleIdentity(ctx context.Context, writer client.Client, reader client.Reader, identityName, roleARN, sourceIdentityName, allowedNamespace string, dryRun bool) (bool, error) {
	if reader == nil || identityName == "" || roleARN == "" {
		return false, nil
	}
	gvk := schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta2",
		Kind:    "AWSClusterRoleIdentity",
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(gvk)
	err := reader.Get(ctx, client.ObjectKey{Name: identityName}, current)
	if apierrors.IsNotFound(err) {
		if !dryRun && writer != nil {
			spec := map[string]any{
				"roleARN": roleARN,
			}
			if allowedNamespace != "" {
				spec["allowedNamespaces"] = map[string]any{
					"list": []any{allowedNamespace},
				}
			}
			if sourceIdentityName != "" {
				spec["sourceIdentityRef"] = map[string]any{
					"kind": "AWSClusterStaticIdentity",
					"name": sourceIdentityName,
				}
			}
			current.SetName(identityName)
			current.Object["spec"] = spec
			if err := writer.Create(ctx, current); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	existingARN, _, _ := unstructured.NestedString(current.Object, "spec", "roleARN")
	if existingARN == roleARN {
		return false, nil
	}
	if !dryRun && writer != nil {
		if err := unstructured.SetNestedField(current.Object, roleARN, "spec", "roleARN"); err != nil {
			return false, err
		}
		if err := writer.Update(ctx, current); err != nil {
			return false, err
		}
	}
	return true, nil
}

func oldestNonDefaultVersion(versions []PolicyVersion) string {
	var nonDefault []PolicyVersion
	for _, v := range versions {
		if !v.IsDefaultVersion && v.VersionID != "" {
			nonDefault = append(nonDefault, v)
		}
	}
	if len(nonDefault) == 0 {
		return ""
	}
	sort.Slice(nonDefault, func(i, j int) bool {
		if nonDefault[i].CreateDate != nonDefault[j].CreateDate {
			return nonDefault[i].CreateDate < nonDefault[j].CreateDate
		}
		return versionNumber(nonDefault[i].VersionID) < versionNumber(nonDefault[j].VersionID)
	})
	return nonDefault[0].VersionID
}

func versionNumber(v string) int {
	v = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(v)), "v")
	var n int
	_, _ = fmt.Sscanf(v, "%d", &n)
	return n
}

func normalizeJSON(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	inQuotes := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			b.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' && inQuotes {
			b.WriteByte(c)
			escaped = true
			continue
		}
		if c == '"' {
			inQuotes = !inQuotes
			b.WriteByte(c)
			continue
		}
		if !inQuotes && (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
