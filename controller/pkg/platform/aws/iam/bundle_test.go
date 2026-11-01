package iam

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEmbeddedPoliciesMatchDocsIAM(t *testing.T) {
	entries, err := policyFS.ReadDir("policies")
	if err != nil {
		t.Fatalf("ReadDir(policies): %v", err)
	}
	docsDir := filepath.Join("..", "..", "..", "..", "..", "docs", "iam")
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		embeddedBytes, err := policyFS.ReadFile("policies/" + entry.Name())
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", entry.Name(), err)
		}
		docsBytes, err := os.ReadFile(filepath.Join(docsDir, entry.Name()))
		if err != nil {
			t.Fatalf("docs/iam/%s missing or unreadable: %v", entry.Name(), err)
		}
		if normalizeJSON(string(embeddedBytes)) != normalizeJSON(string(docsBytes)) {
			t.Errorf("embedded policies/%s differs from docs/iam/%s", entry.Name(), entry.Name())
		}
	}
}

func TestRenderFullLinuxWindowsVPCCNIAMIObservabilityBundle(t *testing.T) {
	opts := Options{
		AccountID:             "123456789012",
		Region:                "us-west-2",
		ClusterName:           "prod-hybrid",
		AMIs:                  []string{"ami-linuxamd64", "ami-linuxarm64", "ami-win2022", "ami-win2025"},
		Subnets:               []string{"subnet-aaa111", "subnet-bbb222"},
		SecurityGroups:        []string{"sg-111", "sg-222"},
		AssetBucket:           "prod-hybrid-assets",
		AssetPrefix:           "bootstrap/",
		IncludeWindows:        true,
		IncludeVPCCNI:         true,
		VPCCNIFamily:          "dual",
		IncludeAMILifecycle:   true,
		IncludeAMIPerformance: true,
		IncludeObservability:  true,
	}
	bundle, err := Render(opts)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	if len(bundle.Policies) != 10 {
		t.Fatalf("expected 10 managed policies in full bundle, got %d", len(bundle.Policies))
	}
	for _, pol := range bundle.Policies {
		if pol.CompactSize > ManagedPolicyLimitBytes {
			t.Errorf("policy %s compact size %d > %d", pol.Name, pol.CompactSize, ManagedPolicyLimitBytes)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(pol.Document), &doc); err != nil {
			t.Errorf("policy %s produced invalid JSON: %v", pol.Name, err)
		}
		if pol.ID == PolicyCAPAProvisioning {
			for _, wantAMI := range opts.AMIs {
				if !strings.Contains(pol.Document, "image/"+wantAMI) {
					t.Errorf("capa-provisioning policy missing AMI %s", wantAMI)
				}
			}
		}
	}
	if len(bundle.Roles) != 4 {
		t.Fatalf("expected 4 roles (capa, controller, worker, operator), got %d", len(bundle.Roles))
	}
	// Verify controller role gets IPv4 VPC CNI policy when Windows + VPC CNI is enabled
	var ctrlRole, workerRole RoleSpec
	for _, r := range bundle.Roles {
		if r.Name == "prod-hybrid-endpoint-controller" {
			ctrlRole = r
		}
		if r.Name == "prod-hybrid-worker-node" {
			workerRole = r
		}
	}
	if !slices.Contains(ctrlRole.ManagedPolicyNames, "prod-hybrid-vpc-cni-node-ipv4") {
		t.Errorf("expected controller role to include vpc-cni-node-ipv4 for Windows VPC IPAM, got %v", ctrlRole.ManagedPolicyNames)
	}
	if !slices.Contains(workerRole.ManagedPolicyARNs, SSMManagedInstanceCoreARN) {
		t.Errorf("expected worker role to include AmazonSSMManagedInstanceCore when observability is enabled, got %v", workerRole.ManagedPolicyARNs)
	}
}

func TestRenderAccountWideCAPIBundleWithAWSManagedPolicies(t *testing.T) {
	bundle, err := Render(Options{
		Scope:                 "account",
		AccountID:             "123456789012",
		IncludeWindows:        true,
		IncludeVPCCNI:         true,
		IncludeAMILifecycle:   true,
		IncludeAMIPerformance: true,
		IncludeObservability:  true,
	})
	if err != nil {
		t.Fatalf("Render(account): %v", err)
	}
	if bundle.Scope != "account" {
		t.Fatalf("expected scope=account, got %q", bundle.Scope)
	}
	if len(bundle.Policies) != 3 {
		t.Fatalf("expected 3 customer-managed delta policies in account-wide bundle, got %d", len(bundle.Policies))
	}
	for _, pol := range bundle.Policies {
		if pol.CompactSize > ManagedPolicyLimitBytes {
			t.Errorf("policy %s compact size %d > %d", pol.Name, pol.CompactSize, ManagedPolicyLimitBytes)
		}
	}
	if bundle.InstanceProfile.Name != AccountNodesRoleName {
		t.Errorf("expected clusterawsadm instance profile %s, got %s", AccountNodesRoleName, bundle.InstanceProfile.Name)
	}
	var ctrlRole, nodeRole, amiRole RoleSpec
	for _, r := range bundle.Roles {
		switch r.Name {
		case AccountControllersRoleName:
			ctrlRole = r
		case AccountNodesRoleName:
			nodeRole = r
		case AccountAMIOperatorRoleName:
			amiRole = r
		}
	}
	if !slices.Contains(ctrlRole.ManagedPolicyARNs, AWSManagedWindowsVPCResourceControllerARN) {
		t.Errorf("expected controllers role to attach %s, got %v", AWSManagedWindowsVPCResourceControllerARN, ctrlRole.ManagedPolicyARNs)
	}
	for _, wantARN := range []string{
		AWSManagedVPCCNIPolicyARN,
		AWSManagedECRReadOnlyARN,
		SSMManagedInstanceCoreARN,
		AWSManagedCloudWatchAgentARN,
	} {
		if !slices.Contains(nodeRole.ManagedPolicyARNs, wantARN) {
			t.Errorf("expected nodes role to attach AWS-Managed Policy %s, got %v", wantARN, nodeRole.ManagedPolicyARNs)
		}
	}
	if !slices.Contains(amiRole.ManagedPolicyARNs, AWSManagedImageBuilderARN) {
		t.Errorf("expected ami-operator role to attach %s, got %v", AWSManagedImageBuilderARN, amiRole.ManagedPolicyARNs)
	}
}

type fakeIAM struct {
	policies         map[string]string
	policyVersions   map[string][]PolicyVersion
	roles            map[string]string
	roleAttachments  map[string][]string
	instanceProfiles map[string]*InstanceProfile
	deletedVersions  []string
}

func newFakeIAM() *fakeIAM {
	return &fakeIAM{
		policies:         map[string]string{},
		policyVersions:   map[string][]PolicyVersion{},
		roles:            map[string]string{},
		roleAttachments:  map[string][]string{},
		instanceProfiles: map[string]*InstanceProfile{},
	}
}

func (f *fakeIAM) GetPolicy(_ context.Context, arn string) (bool, string, error) {
	doc, ok := f.policies[arn]
	if !ok || doc == "" {
		return false, "", nil
	}
	for _, v := range f.policyVersions[arn] {
		if v.IsDefaultVersion {
			return true, v.VersionID, nil
		}
	}
	return true, "v1", nil
}

func (f *fakeIAM) GetPolicyVersionDocument(_ context.Context, arn, _ string) (string, error) {
	return f.policies[arn], nil
}

func (f *fakeIAM) CreatePolicy(_ context.Context, name, _, document string) (string, error) {
	arn := "arn:aws:iam::123456789012:policy/" + name
	f.policies[arn] = document
	f.policyVersions[arn] = []PolicyVersion{{VersionID: "v1", IsDefaultVersion: true}}
	return arn, nil
}

func (f *fakeIAM) ListPolicyVersions(_ context.Context, arn string) ([]PolicyVersion, error) {
	return f.policyVersions[arn], nil
}

func (f *fakeIAM) DeletePolicyVersion(_ context.Context, arn, versionID string) error {
	f.deletedVersions = append(f.deletedVersions, arn+"@"+versionID)
	var kept []PolicyVersion
	for _, v := range f.policyVersions[arn] {
		if v.VersionID != versionID {
			kept = append(kept, v)
		}
	}
	f.policyVersions[arn] = kept
	return nil
}

func (f *fakeIAM) CreatePolicyVersion(_ context.Context, arn, document string, setAsDefault bool) (string, error) {
	f.policies[arn] = document
	nextVer := "v6"
	for i := range f.policyVersions[arn] {
		if setAsDefault {
			f.policyVersions[arn][i].IsDefaultVersion = false
		}
	}
	f.policyVersions[arn] = append(f.policyVersions[arn], PolicyVersion{
		VersionID:        nextVer,
		IsDefaultVersion: setAsDefault,
	})
	return nextVer, nil
}

func (f *fakeIAM) GetRole(_ context.Context, name string) (bool, string, error) {
	doc, ok := f.roles[name]
	return ok, doc, nil
}

func (f *fakeIAM) CreateRole(_ context.Context, name, _, assumeRoleDoc string) (string, error) {
	f.roles[name] = assumeRoleDoc
	return "arn:aws:iam::123456789012:role/" + name, nil
}

func (f *fakeIAM) UpdateAssumeRolePolicy(_ context.Context, name, assumeRoleDoc string) error {
	f.roles[name] = assumeRoleDoc
	return nil
}

func (f *fakeIAM) ListAttachedRolePolicies(_ context.Context, roleName string) ([]string, error) {
	return f.roleAttachments[roleName], nil
}

func (f *fakeIAM) AttachRolePolicy(_ context.Context, roleName, policyARN string) error {
	f.roleAttachments[roleName] = append(f.roleAttachments[roleName], policyARN)
	return nil
}

func (f *fakeIAM) GetInstanceProfile(_ context.Context, name string) (*InstanceProfile, error) {
	return f.instanceProfiles[name], nil
}

func (f *fakeIAM) CreateInstanceProfile(_ context.Context, name string) (string, error) {
	arn := "arn:aws:iam::123456789012:instance-profile/" + name
	f.instanceProfiles[name] = &InstanceProfile{Name: name, ARN: arn}
	return arn, nil
}

func (f *fakeIAM) AddRoleToInstanceProfile(_ context.Context, profileName, roleName string) error {
	prof := f.instanceProfiles[profileName]
	prof.RoleNames = append(prof.RoleNames, roleName)
	return nil
}

func TestReconcileAndAuditIAMBundleWithVersionPruning(t *testing.T) {
	ctx := context.Background()
	bundle, err := Render(Options{
		AccountID:             "123456789012",
		Region:                "us-west-2",
		ClusterName:           "site-a",
		IncludeWindows:        true,
		IncludeVPCCNI:         true,
		IncludeAMILifecycle:   true,
		IncludeAMIPerformance: true,
		IncludeObservability:  true,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	api := newFakeIAM()
	// Pre-populate one policy with 5 versions and stale document to exercise version pruning
	capaPolARN := bundle.Policies[0].ARN
	api.policies[capaPolARN] = `{"Version":"2012-10-17","Statement":[]}`
	api.policyVersions[capaPolARN] = []PolicyVersion{
		{VersionID: "v1", IsDefaultVersion: false, CreateDate: "2026-01-01T00:00:00Z"},
		{VersionID: "v2", IsDefaultVersion: false, CreateDate: "2026-01-02T00:00:00Z"},
		{VersionID: "v3", IsDefaultVersion: false, CreateDate: "2026-01-03T00:00:00Z"},
		{VersionID: "v4", IsDefaultVersion: false, CreateDate: "2026-01-04T00:00:00Z"},
		{VersionID: "v5", IsDefaultVersion: true, CreateDate: "2026-01-05T00:00:00Z"},
	}

	// Before reconcile, Audit should report missing items
	findings, err := Audit(ctx, api, bundle)
	if err != nil {
		t.Fatalf("Audit before reconcile: %v", err)
	}
	if len(findings) == 0 {
		t.Fatalf("expected Audit findings before reconcile")
	}

	report, err := Reconcile(ctx, api, bundle, false)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.PoliciesUpdated) != 1 || report.PoliciesUpdated[0] != capaPolARN {
		t.Errorf("expected capa policy to be updated, got %v", report.PoliciesUpdated)
	}
	if !slices.Contains(api.deletedVersions, capaPolARN+"@v1") {
		t.Errorf("expected oldest non-default version v1 to be pruned, got %v", api.deletedVersions)
	}
	if len(report.RolesCreated) != 4 {
		t.Errorf("expected 4 roles created, got %v", report.RolesCreated)
	}

	// Audit after reconcile should report 0 findings
	findings, err = Audit(ctx, api, bundle)
	if err != nil {
		t.Fatalf("Audit after reconcile: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 Audit findings after reconcile, got %v", findings)
	}

	// Second Reconcile should be a no-op
	report2, err := Reconcile(ctx, api, bundle, false)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if len(report2.Changes) != 0 {
		t.Errorf("expected second Reconcile to be idempotent with 0 changes, got %v", report2.Changes)
	}
}

func TestDiscoverOptionsAndReconcileClusterRoleIdentity(t *testing.T) {
	ctx := context.Background()
	sc := runtime.NewScheme()
	for _, kind := range []string{"AWSCluster", "AWSMachineTemplate", "AWSClusterRoleIdentity"} {
		gvk := schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: kind}
		sc.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		sc.AddKnownTypeWithName(gvk.GroupVersion().WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	awsCluster := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
			"kind":       "AWSCluster",
			"metadata": map[string]any{
				"name":      "site-aws",
				"namespace": "cloud-provisioning",
			},
			"spec": map[string]any{
				"region": "us-west-2",
				"network": map[string]any{
					"subnets": []any{
						map[string]any{"resourceID": "subnet-012345"},
					},
				},
			},
		},
	}
	awsTemplate := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
			"kind":       "AWSMachineTemplate",
			"metadata": map[string]any{
				"name":      "public-worker",
				"namespace": "cloud-provisioning",
			},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"ami":                map[string]any{"id": "ami-0abcdef"},
						"iamInstanceProfile": "site-aws-worker",
						"additionalSecurityGroups": []any{
							map[string]any{"id": "sg-098765"},
						},
					},
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(sc).WithObjects(awsCluster, awsTemplate).Build()

	var opts Options
	if err := DiscoverOptionsFromCluster(ctx, cl, &opts); err != nil {
		t.Fatalf("DiscoverOptionsFromCluster: %v", err)
	}
	if opts.ClusterName != "site-aws" || opts.Region != "us-west-2" {
		t.Errorf("unexpected discovered cluster/region: %+v", opts)
	}
	if !slices.Contains(opts.Subnets, "subnet-012345") || !slices.Contains(opts.AMIs, "ami-0abcdef") || !slices.Contains(opts.SecurityGroups, "sg-098765") {
		t.Errorf("unexpected discovered subnets/AMIs/SGs: %+v", opts)
	}
	if opts.InstanceProfileName != "site-aws-worker" {
		t.Errorf("expected discovered instanceProfileName site-aws-worker, got %s", opts.InstanceProfileName)
	}

	changed, err := ReconcileAWSClusterRoleIdentity(ctx, cl, cl, "site-capa-role", "arn:aws:iam::123456789012:role/site-aws-capa-controller", "aws-static-creds", "cloud-provisioning", false)
	if err != nil || !changed {
		t.Fatalf("ReconcileAWSClusterRoleIdentity: changed=%v err=%v", changed, err)
	}
	identity := &unstructured.Unstructured{}
	identity.SetGroupVersionKind(schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSClusterRoleIdentity"})
	if err := cl.Get(ctx, client.ObjectKey{Name: "site-capa-role"}, identity); err != nil {
		t.Fatalf("getting created AWSClusterRoleIdentity: %v", err)
	}
	roleARN, _, _ := unstructured.NestedString(identity.Object, "spec", "roleARN")
	if roleARN != "arn:aws:iam::123456789012:role/site-aws-capa-controller" {
		t.Errorf("unexpected roleARN %s", roleARN)
	}
}
