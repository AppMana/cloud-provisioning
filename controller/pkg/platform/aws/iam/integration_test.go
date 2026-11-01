package iam

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"slices"
	"testing"
	"time"
)

func TestLiveAWSIAMAccountBundleLifecycleMigrationAndCleanup(t *testing.T) {
	if os.Getenv("CLOUD_PROVISIONING_AWS_IAM_INTEGRATION") != "1" {
		t.Skip("skipping live AWS IAM integration test (set CLOUD_PROVISIONING_AWS_IAM_INTEGRATION=1 to run)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client, accountID, err := NewClientFromDefaultConfig(ctx, "us-west-2")
	if err != nil {
		t.Fatalf("NewClientFromDefaultConfig: %v", err)
	}
	if accountID == "" {
		t.Fatalf("expected non-empty AWS accountID from STS")
	}

	var randBytes [3]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	suffix := hex.EncodeToString(randBytes[:])
	prefix := "cp-it-" + suffix
	legacyBundleID := "cp-leg-" + suffix

	bundle, err := Render(Options{
		Scope:                 "account",
		AccountID:             accountID,
		RolePrefix:            prefix,
		MigrateFromBundles:    []string{legacyBundleID},
		IncludeWindows:        true,
		IncludeVPCCNI:         true,
		IncludeAMILifecycle:   true,
		IncludeAMIPerformance: true,
		IncludeObservability:  true,
		Tags: map[string]string{
			"cloud-provisioning.appmana.com/test-suite": "iam-integration",
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Ensure every role and policy name fits AWS IAM's 64-character limit
	for _, r := range bundle.Roles {
		if len(r.Name) > 64 {
			t.Fatalf("role name %q length %d exceeds AWS IAM 64-char limit", r.Name, len(r.Name))
		}
	}
	for _, p := range bundle.Policies {
		if len(p.Name) > 64 {
			t.Fatalf("policy name %q length %d exceeds AWS IAM 64-char limit", p.Name, len(p.Name))
		}
	}

	orphanPolicyName := legacyBundleID + "-obsolete-policy"
	orphanPolicyARN := "arn:aws:iam::" + accountID + ":policy/" + orphanPolicyName

	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanCancel()
		_, _ = Cleanup(cleanCtx, client, bundle, false)
		_ = deleteCustomerManagedPolicyWithVersions(cleanCtx, client, orphanPolicyARN)
	})

	// 1. Reconcile full account-wide bundle (3 Customer Managed delta policies + 6 AWS-Managed Policies + 3 Roles + 1 Instance Profile)
	t.Logf("Reconciling live AWS IAM bundle %s in account %s...", bundle.BundleID, accountID)
	rep1, err := Reconcile(ctx, client, bundle, false)
	if err != nil {
		t.Fatalf("initial Reconcile failed: %v", err)
	}
	if len(rep1.PoliciesCreated) != len(bundle.Policies) {
		t.Errorf("expected %d policies created, got %v", len(bundle.Policies), rep1.PoliciesCreated)
	}
	if len(rep1.RolesCreated) != len(bundle.Roles) {
		t.Errorf("expected %d roles created, got %v", len(bundle.Roles), rep1.RolesCreated)
	}

	// 2. Audit live AWS IAM state and verify 0 findings
	findings, err := Audit(ctx, client, bundle)
	if err != nil {
		t.Fatalf("Audit after initial Reconcile failed: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 Audit findings after Reconcile, got %v", findings)
	}

	// 3. Reconcile a second time and verify 0 changes (idempotency)
	rep2, err := Reconcile(ctx, client, bundle, false)
	if err != nil {
		t.Fatalf("second Reconcile failed: %v", err)
	}
	if len(rep2.Changes) != 0 {
		t.Errorf("expected second Reconcile to be a no-op (0 changes), got %v", rep2.Changes)
	}

	// 4. Simulate migration & tag drift:
	//    - Create an obsolete Customer Managed Policy tagged with legacyBundleID and attach it to the controllers role.
	//    - Mutate a schema tag on the nodes role.
	ctrlRoleName := bundle.Roles[0].Name
	nodeRole := bundle.Roles[1]
	_, err = client.CreatePolicy(ctx, orphanPolicyName, "Obsolete policy for migration pruning test", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:DescribeRegions","Resource":"*"}]}`, map[string]string{
		TagManagedBy:     ManagedByValue,
		TagBundleID:      legacyBundleID,
		TagSchemaVersion: "v0",
	})
	if err != nil {
		t.Fatalf("creating legacy orphan policy %s: %v", orphanPolicyName, err)
	}
	if err := client.AttachRolePolicy(ctx, ctrlRoleName, orphanPolicyARN); err != nil {
		t.Fatalf("attaching legacy orphan policy to %s: %v", ctrlRoleName, err)
	}
	if err := client.TagRole(ctx, nodeRole.Name, map[string]string{TagSchemaVersion: "v0-drifted"}); err != nil {
		t.Fatalf("drifting tag on %s: %v", nodeRole.Name, err)
	}

	// Verify Audit catches the drifted tag on nodeRole
	driftFindings, err := Audit(ctx, client, bundle)
	if err != nil || len(driftFindings) == 0 {
		t.Fatalf("expected Audit to report drifted tag on %s, got %v (err=%v)", nodeRole.Name, driftFindings, err)
	}

	// Run Reconcile to repair tag drift and prune the legacy orphan policy
	rep3, err := Reconcile(ctx, client, bundle, false)
	if err != nil {
		t.Fatalf("migration Reconcile failed: %v", err)
	}
	if !slices.Contains(rep3.PoliciesPruned, orphanPolicyARN) {
		t.Errorf("expected orphan policy %s to be pruned, got %v", orphanPolicyARN, rep3.PoliciesPruned)
	}
	if !slices.Contains(rep3.TagsReconciled, nodeRole.ARN) {
		t.Errorf("expected drifted role %s tags to be reconciled, got %v", nodeRole.ARN, rep3.TagsReconciled)
	}
	exists, _, err := client.GetPolicy(ctx, orphanPolicyARN)
	if err != nil || exists {
		t.Errorf("expected pruned orphan policy %s to be deleted in AWS IAM (exists=%v, err=%v)", orphanPolicyARN, exists, err)
	}

	// 5. Run Cleanup and verify all created resources are removed from AWS IAM
	cleanRep, err := Cleanup(ctx, client, bundle, false)
	if err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}
	if len(cleanRep.InstanceProfilesDeleted) != 1 {
		t.Errorf("expected 1 instance profile deleted, got %v", cleanRep.InstanceProfilesDeleted)
	}
	if len(cleanRep.RolesDeleted) != len(bundle.Roles) {
		t.Errorf("expected %d roles deleted, got %v", len(bundle.Roles), cleanRep.RolesDeleted)
	}
	if len(cleanRep.PoliciesDeleted) != len(bundle.Policies) {
		t.Errorf("expected %d policies deleted, got %v", len(bundle.Policies), cleanRep.PoliciesDeleted)
	}

	// Confirm absence in live AWS IAM
	if prof, err := client.GetInstanceProfile(ctx, bundle.InstanceProfile.Name); err != nil || prof != nil {
		t.Errorf("expected instance profile %s to be absent after Cleanup, got %+v (err=%v)", bundle.InstanceProfile.Name, prof, err)
	}
	for _, r := range bundle.Roles {
		if exists, _, err := client.GetRole(ctx, r.Name); err != nil || exists {
			t.Errorf("expected role %s to be absent after Cleanup (exists=%v, err=%v)", r.Name, exists, err)
		}
	}
	for _, p := range bundle.Policies {
		if exists, _, err := client.GetPolicy(ctx, p.ARN); err != nil || exists {
			t.Errorf("expected policy %s to be absent after Cleanup (exists=%v, err=%v)", p.ARN, exists, err)
		}
	}
	t.Logf("Live AWS IAM integration test passed: created, audited, migrated/pruned, and cleaned up %d policies, %d roles, and 1 instance profile.", len(bundle.Policies), len(bundle.Roles))
}
