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

// API abstracts the AWS IAM operations needed at installation, migration, and
// cleanup time to create, update, tag, audit, and remove Customer Managed
// Policies, Roles, and Instance Profiles.
type API interface {
	GetPolicy(ctx context.Context, arn string) (exists bool, defaultVersionID string, err error)
	GetPolicyVersionDocument(ctx context.Context, arn, versionID string) (string, error)
	CreatePolicy(ctx context.Context, name, description, document string, tags map[string]string) (arn string, err error)
	ListPolicyVersions(ctx context.Context, arn string) ([]PolicyVersion, error)
	DeletePolicyVersion(ctx context.Context, arn, versionID string) error
	CreatePolicyVersion(ctx context.Context, arn, document string, setAsDefault bool) (versionID string, err error)
	ListPolicyTags(ctx context.Context, arn string) (map[string]string, error)
	TagPolicy(ctx context.Context, arn string, tags map[string]string) error
	DeletePolicy(ctx context.Context, arn string) error

	GetRole(ctx context.Context, name string) (exists bool, assumeRoleDoc string, err error)
	CreateRole(ctx context.Context, name, description, assumeRoleDoc string, tags map[string]string) (arn string, err error)
	UpdateAssumeRolePolicy(ctx context.Context, name, assumeRoleDoc string) error
	ListAttachedRolePolicies(ctx context.Context, roleName string) (policyARNs []string, err error)
	AttachRolePolicy(ctx context.Context, roleName, policyARN string) error
	DetachRolePolicy(ctx context.Context, roleName, policyARN string) error
	ListRoleTags(ctx context.Context, name string) (map[string]string, error)
	TagRole(ctx context.Context, name string, tags map[string]string) error
	DeleteRole(ctx context.Context, name string) error

	GetInstanceProfile(ctx context.Context, name string) (*InstanceProfile, error)
	CreateInstanceProfile(ctx context.Context, name string, tags map[string]string) (arn string, err error)
	AddRoleToInstanceProfile(ctx context.Context, profileName, roleName string) error
	RemoveRoleFromInstanceProfile(ctx context.Context, profileName, roleName string) error
	ListInstanceProfileTags(ctx context.Context, name string) (map[string]string, error)
	TagInstanceProfile(ctx context.Context, name string, tags map[string]string) error
	DeleteInstanceProfile(ctx context.Context, name string) error
}

// ReconcileReport summarizes the IAM policies, roles, instance profile, tags,
// pruned legacy resources, and Kubernetes identity resources reconciled.
type ReconcileReport struct {
	DryRun                 bool     `json:"dryRun,omitempty"`
	Scope                  string   `json:"scope,omitempty"`
	BundleID               string   `json:"bundleID,omitempty"`
	AccountID              string   `json:"accountID"`
	Region                 string   `json:"region"`
	ClusterName            string   `json:"clusterName,omitempty"`
	PoliciesCreated        []string `json:"policiesCreated,omitempty"`
	PoliciesUpdated        []string `json:"policiesUpdated,omitempty"`
	PoliciesPruned         []string `json:"policiesPruned,omitempty"`
	RolesCreated           []string `json:"rolesCreated,omitempty"`
	RolesUpdated           []string `json:"rolesUpdated,omitempty"`
	RolesPruned            []string `json:"rolesPruned,omitempty"`
	TagsReconciled         []string `json:"tagsReconciled,omitempty"`
	InstanceProfileEnsured string   `json:"instanceProfileEnsured,omitempty"`
	ClusterIdentityUpdated string   `json:"clusterIdentityUpdated,omitempty"`
	Changes                []string `json:"changes,omitempty"`
}

// CleanupReport summarizes the IAM resources safely removed or skipped during Cleanup.
type CleanupReport struct {
	DryRun                  bool     `json:"dryRun,omitempty"`
	BundleID                string   `json:"bundleID"`
	AccountID               string   `json:"accountID"`
	InstanceProfilesDeleted []string `json:"instanceProfilesDeleted,omitempty"`
	RolesDeleted            []string `json:"rolesDeleted,omitempty"`
	PoliciesDeleted         []string `json:"policiesDeleted,omitempty"`
	SkippedUnowned          []string `json:"skippedUnowned,omitempty"`
	Changes                 []string `json:"changes,omitempty"`
}

// Reconcile idempotently creates or updates all Customer Managed Policies, IAM Roles,
// policy attachments, canonical tags, and the EC2 Instance Profile in the Bundle,
// and prunes obsolete Customer Managed Policies belonging to the same or migrated bundle.
func Reconcile(ctx context.Context, api API, bundle *Bundle, dryRun bool) (*ReconcileReport, error) {
	if bundle == nil {
		return nil, fmt.Errorf("IAM bundle is required")
	}
	if api == nil && !dryRun {
		return nil, fmt.Errorf("IAM API client is required when dryRun is false")
	}
	report := &ReconcileReport{
		DryRun:      dryRun,
		Scope:       bundle.Scope,
		BundleID:    bundle.BundleID,
		AccountID:   bundle.AccountID,
		Region:      bundle.Region,
		ClusterName: bundle.ClusterName,
	}

	desiredPolicyARNs := map[string]bool{}
	for _, pol := range bundle.Policies {
		desiredPolicyARNs[pol.ARN] = true
	}

	// 1. Reconcile Customer Managed Policies + Tags
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
				if _, err := api.CreatePolicy(ctx, pol.Name, pol.Description, pol.Document, pol.Tags); err != nil {
					return report, fmt.Errorf("creating IAM policy %s: %w", pol.Name, err)
				}
			}
			report.PoliciesCreated = append(report.PoliciesCreated, pol.ARN)
			report.Changes = append(report.Changes, fmt.Sprintf("created customer managed policy %s (%d bytes)", pol.ARN, pol.CompactSize))
			continue
		}
		currentDoc, err := api.GetPolicyVersionDocument(ctx, pol.ARN, defaultVer)
		if err != nil || normalizeJSON(currentDoc) != normalizeJSON(pol.Document) {
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
		if len(pol.Tags) > 0 {
			existingTags, err := api.ListPolicyTags(ctx, pol.ARN)
			if err != nil {
				return report, fmt.Errorf("listing tags on IAM policy %s: %w", pol.ARN, err)
			}
			if diff := missingOrChangedTags(existingTags, pol.Tags); len(diff) > 0 {
				if !dryRun {
					if err := api.TagPolicy(ctx, pol.ARN, diff); err != nil {
						return report, fmt.Errorf("tagging IAM policy %s: %w", pol.ARN, err)
					}
				}
				report.TagsReconciled = append(report.TagsReconciled, pol.ARN)
				report.Changes = append(report.Changes, fmt.Sprintf("reconciled %d tag(s) on customer managed policy %s", len(diff), pol.Name))
			}
		}
	}

	// 2. Reconcile IAM Roles, Trust Policies, Tags, Policy Attachments, and Orphan Pruning
	var candidateOrphanPolicies []string
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
				if _, err := api.CreateRole(ctx, role.Name, role.Description, role.AssumeRolePolicyDoc, role.Tags); err != nil {
					return report, fmt.Errorf("creating IAM role %s: %w", role.Name, err)
				}
			}
			report.RolesCreated = append(report.RolesCreated, role.ARN)
			report.Changes = append(report.Changes, fmt.Sprintf("created IAM role %s", role.ARN))
		} else {
			if normalizeJSON(currentTrust) != normalizeJSON(role.AssumeRolePolicyDoc) {
				if !dryRun {
					if err := api.UpdateAssumeRolePolicy(ctx, role.Name, role.AssumeRolePolicyDoc); err != nil {
						return report, fmt.Errorf("updating trust policy on IAM role %s: %w", role.Name, err)
					}
				}
				report.RolesUpdated = append(report.RolesUpdated, role.ARN)
				report.Changes = append(report.Changes, fmt.Sprintf("updated assume-role trust policy on IAM role %s", role.Name))
			}
			if len(role.Tags) > 0 {
				existingTags, err := api.ListRoleTags(ctx, role.Name)
				if err != nil {
					return report, fmt.Errorf("listing tags on IAM role %s: %w", role.Name, err)
				}
				if diff := missingOrChangedTags(existingTags, role.Tags); len(diff) > 0 {
					if !dryRun {
						if err := api.TagRole(ctx, role.Name, diff); err != nil {
							return report, fmt.Errorf("tagging IAM role %s: %w", role.Name, err)
						}
					}
					report.TagsReconciled = append(report.TagsReconciled, role.ARN)
					report.Changes = append(report.Changes, fmt.Sprintf("reconciled %d tag(s) on IAM role %s", len(diff), role.Name))
				}
			}
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

		// Check attached policies that are no longer desired on this role: if they are
		// Customer Managed Policies tagged as owned by this bundle (or a migrated bundle),
		// detach them and queue them for safe orphan deletion.
		for _, attachedARN := range attached {
			if slices.Contains(role.ManagedPolicyARNs, attachedARN) || isAWSManagedPolicyARN(attachedARN) {
				continue
			}
			polTags, err := api.ListPolicyTags(ctx, attachedARN)
			if err != nil || !isOwnedByBundleOrMigration(polTags, bundle) {
				continue
			}
			if !dryRun {
				if err := api.DetachRolePolicy(ctx, role.Name, attachedARN); err != nil {
					return report, fmt.Errorf("detaching obsolete policy %s from role %s: %w", attachedARN, role.Name, err)
				}
			}
			report.Changes = append(report.Changes, fmt.Sprintf("detached obsolete managed policy %s from IAM role %s", attachedARN, role.Name))
			if !desiredPolicyARNs[attachedARN] && !slices.Contains(candidateOrphanPolicies, attachedARN) {
				candidateOrphanPolicies = append(candidateOrphanPolicies, attachedARN)
			}
		}
	}

	// Delete any orphaned Customer Managed Policies that were detached from roles in this bundle
	for _, orphanARN := range candidateOrphanPolicies {
		if !dryRun {
			if err := deleteCustomerManagedPolicyWithVersions(ctx, api, orphanARN); err != nil {
				return report, fmt.Errorf("deleting orphaned policy %s: %w", orphanARN, err)
			}
		}
		report.PoliciesPruned = append(report.PoliciesPruned, orphanARN)
		report.Changes = append(report.Changes, fmt.Sprintf("pruned obsolete customer managed policy %s", orphanARN))
	}

	// 3. Reconcile Worker EC2 Instance Profile + Tags
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
				if _, err := api.CreateInstanceProfile(ctx, prof.Name, prof.Tags); err != nil {
					return report, fmt.Errorf("creating instance profile %s: %w", prof.Name, err)
				}
			}
			report.Changes = append(report.Changes, fmt.Sprintf("created EC2 instance profile %s", prof.ARN))
		} else if len(prof.Tags) > 0 {
			existingTags, err := api.ListInstanceProfileTags(ctx, prof.Name)
			if err != nil {
				return report, fmt.Errorf("listing tags on instance profile %s: %w", prof.Name, err)
			}
			if diff := missingOrChangedTags(existingTags, prof.Tags); len(diff) > 0 {
				if !dryRun {
					if err := api.TagInstanceProfile(ctx, prof.Name, diff); err != nil {
						return report, fmt.Errorf("tagging instance profile %s: %w", prof.Name, err)
					}
				}
				report.TagsReconciled = append(report.TagsReconciled, prof.ARN)
				report.Changes = append(report.Changes, fmt.Sprintf("reconciled %d tag(s) on EC2 instance profile %s", len(diff), prof.Name))
			}
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

// Cleanup safely tears down all IAM resources in the Bundle in reverse dependency
// order (Instance Profile -> Roles -> Customer Managed Policies), verifying on every
// resource that its live AWS IAM tags match cloud-provisioning.appmana.com/managed-by=cloud-provisioning
// and cloud-provisioning.appmana.com/iam-bundle=<bundle.BundleID>. Resources lacking
// matching ownership tags are never deleted.
func Cleanup(ctx context.Context, api API, bundle *Bundle, dryRun bool) (*CleanupReport, error) {
	if bundle == nil {
		return nil, fmt.Errorf("IAM bundle is required")
	}
	if api == nil && !dryRun {
		return nil, fmt.Errorf("IAM API client is required when dryRun is false")
	}
	report := &CleanupReport{
		DryRun:    dryRun,
		BundleID:  bundle.BundleID,
		AccountID: bundle.AccountID,
	}
	if api == nil {
		return report, nil
	}

	// 1. Instance Profile
	if bundle.InstanceProfile.Name != "" {
		prof, err := api.GetInstanceProfile(ctx, bundle.InstanceProfile.Name)
		if err != nil {
			return report, err
		}
		if prof != nil {
			tags, err := api.ListInstanceProfileTags(ctx, prof.Name)
			if err != nil {
				return report, err
			}
			if !isOwnedByBundle(tags, bundle.BundleID) {
				report.SkippedUnowned = append(report.SkippedUnowned, prof.ARN)
			} else {
				for _, rName := range prof.RoleNames {
					if !dryRun {
						if err := api.RemoveRoleFromInstanceProfile(ctx, prof.Name, rName); err != nil {
							return report, fmt.Errorf("removing role %s from instance profile %s: %w", rName, prof.Name, err)
						}
					}
					report.Changes = append(report.Changes, fmt.Sprintf("removed role %s from EC2 instance profile %s", rName, prof.Name))
				}
				if !dryRun {
					if err := api.DeleteInstanceProfile(ctx, prof.Name); err != nil {
						return report, fmt.Errorf("deleting instance profile %s: %w", prof.Name, err)
					}
				}
				report.InstanceProfilesDeleted = append(report.InstanceProfilesDeleted, prof.ARN)
				report.Changes = append(report.Changes, fmt.Sprintf("deleted EC2 instance profile %s", prof.ARN))
			}
		}
	}

	// 2. IAM Roles (detach all attached policies first, then delete role)
	for _, role := range bundle.Roles {
		exists, _, err := api.GetRole(ctx, role.Name)
		if err != nil {
			return report, err
		}
		if !exists {
			continue
		}
		tags, err := api.ListRoleTags(ctx, role.Name)
		if err != nil {
			return report, err
		}
		if !isOwnedByBundle(tags, bundle.BundleID) {
			report.SkippedUnowned = append(report.SkippedUnowned, role.ARN)
			continue
		}
		attached, err := api.ListAttachedRolePolicies(ctx, role.Name)
		if err != nil {
			return report, err
		}
		for _, polARN := range attached {
			if !dryRun {
				if err := api.DetachRolePolicy(ctx, role.Name, polARN); err != nil {
					return report, fmt.Errorf("detaching policy %s from role %s: %w", polARN, role.Name, err)
				}
			}
			report.Changes = append(report.Changes, fmt.Sprintf("detached policy %s from IAM role %s", polARN, role.Name))
		}
		if !dryRun {
			if err := api.DeleteRole(ctx, role.Name); err != nil {
				return report, fmt.Errorf("deleting role %s: %w", role.Name, err)
			}
		}
		report.RolesDeleted = append(report.RolesDeleted, role.ARN)
		report.Changes = append(report.Changes, fmt.Sprintf("deleted IAM role %s", role.ARN))
	}

	// 3. Customer Managed Policies (delete non-default versions first, then delete policy)
	for _, pol := range bundle.Policies {
		exists, _, err := api.GetPolicy(ctx, pol.ARN)
		if err != nil {
			return report, err
		}
		if !exists {
			continue
		}
		tags, err := api.ListPolicyTags(ctx, pol.ARN)
		if err != nil {
			return report, err
		}
		if !isOwnedByBundle(tags, bundle.BundleID) {
			report.SkippedUnowned = append(report.SkippedUnowned, pol.ARN)
			continue
		}
		if !dryRun {
			if err := deleteCustomerManagedPolicyWithVersions(ctx, api, pol.ARN); err != nil {
				return report, err
			}
		}
		report.PoliciesDeleted = append(report.PoliciesDeleted, pol.ARN)
		report.Changes = append(report.Changes, fmt.Sprintf("deleted customer managed policy %s", pol.ARN))
	}

	return report, nil
}

// Audit inspects live AWS IAM state against the desired Bundle and returns human-readable
// findings (empty slice if all required policies, roles, tags, and instance profiles are in place).
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
			continue
		}
		if len(pol.Tags) > 0 {
			tags, err := api.ListPolicyTags(ctx, pol.ARN)
			if err != nil {
				return nil, err
			}
			if diff := missingOrChangedTags(tags, pol.Tags); len(diff) > 0 {
				findings = append(findings, fmt.Sprintf("Customer Managed Policy %s has %d missing or drifted ownership tag(s)", pol.Name, len(diff)))
			}
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
		if len(role.Tags) > 0 {
			tags, err := api.ListRoleTags(ctx, role.Name)
			if err != nil {
				return nil, err
			}
			if diff := missingOrChangedTags(tags, role.Tags); len(diff) > 0 {
				findings = append(findings, fmt.Sprintf("IAM Role %s has %d missing or drifted ownership tag(s)", role.Name, len(diff)))
			}
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
		} else {
			if !slices.Contains(prof.RoleNames, bundle.InstanceProfile.RoleName) {
				findings = append(findings, fmt.Sprintf("EC2 Instance Profile %s is not bound to role %s", bundle.InstanceProfile.Name, bundle.InstanceProfile.RoleName))
			}
			if len(bundle.InstanceProfile.Tags) > 0 {
				tags, err := api.ListInstanceProfileTags(ctx, bundle.InstanceProfile.Name)
				if err != nil {
					return nil, err
				}
				if diff := missingOrChangedTags(tags, bundle.InstanceProfile.Tags); len(diff) > 0 {
					findings = append(findings, fmt.Sprintf("EC2 Instance Profile %s has %d missing or drifted ownership tag(s)", bundle.InstanceProfile.Name, len(diff)))
				}
			}
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

func missingOrChangedTags(existing, desired map[string]string) map[string]string {
	diff := map[string]string{}
	for k, want := range desired {
		if existing == nil || existing[k] != want {
			diff[k] = want
		}
	}
	return diff
}

func isAWSManagedPolicyARN(arn string) bool {
	return strings.HasPrefix(arn, "arn:aws:iam::aws:policy/")
}

func isOwnedByBundle(tags map[string]string, bundleID string) bool {
	if tags == nil {
		return false
	}
	return tags[TagManagedBy] == ManagedByValue && tags[TagBundleID] == bundleID
}

func isOwnedByBundleOrMigration(tags map[string]string, bundle *Bundle) bool {
	if tags == nil || bundle == nil || tags[TagManagedBy] != ManagedByValue {
		return false
	}
	ownerBundle := tags[TagBundleID]
	if ownerBundle == bundle.BundleID {
		return true
	}
	return slices.Contains(bundle.MigrateFromBundles, ownerBundle)
}

func deleteCustomerManagedPolicyWithVersions(ctx context.Context, api API, arn string) error {
	if isAWSManagedPolicyARN(arn) {
		return fmt.Errorf("refusing to delete AWS-Managed Policy %s", arn)
	}
	versions, err := api.ListPolicyVersions(ctx, arn)
	if err != nil {
		return err
	}
	for _, v := range versions {
		if !v.IsDefaultVersion && v.VersionID != "" {
			if err := api.DeletePolicyVersion(ctx, arn, v.VersionID); err != nil {
				return err
			}
		}
	}
	return api.DeletePolicy(ctx, arn)
}
