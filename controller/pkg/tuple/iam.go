package tuple

import (
	"context"
	"fmt"
	"strings"

	awsiam "github.com/appmana/cloud-provisioning/controller/pkg/platform/aws/iam"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IAMConfig supplies cluster and AWS identifiers to render or reconcile the
// tuple's AWS IAM policies, roles, and instance profile.
type IAMConfig struct {
	Tuple                 string
	Scope                 string
	AccountID             string
	Region                string
	ClusterName           string
	RolePrefix            string
	OwnerTag              string
	OwnerValue            string
	SourcePrincipalARN    string
	AMIs                  []string
	Subnets               []string
	SecurityGroups        []string
	AssetBucket           string
	AssetPrefix           string
	VPCCNIFamily          string
	IncludeAMILifecycle   bool
	IncludeAMIPerformance bool
	IncludeObservability  bool
	ClusterRoleIdentity   string
	SourceIdentityName    string
	AllowedNamespace      string
}

// BuildIAMOptions translates a parsed platform tuple Spec and IAMConfig into
// awsiam.Options, discovering missing cluster/network fields from Kubernetes
// when a reader is provided.
func BuildIAMOptions(ctx context.Context, reader client.Reader, cfg IAMConfig) (awsiam.Options, error) {
	spec, err := Parse(cfg.Tuple)
	if err != nil {
		return awsiam.Options{}, err
	}
	if spec.Provider != "aws" {
		return awsiam.Options{}, fmt.Errorf("platform tuple %q uses provider %q, not aws", spec.String(), spec.Provider)
	}
	includeWindows := spec.IncludesWindows()
	includeVPCCNI := spec.CloudCNI == "aws-vpc"
	fam := strings.TrimSpace(cfg.VPCCNIFamily)
	if fam == "" {
		fam = "dual"
	}
	opts := awsiam.Options{
		Scope:                 cfg.Scope,
		AccountID:             cfg.AccountID,
		Region:                cfg.Region,
		ClusterName:           cfg.ClusterName,
		RolePrefix:            cfg.RolePrefix,
		OwnerTag:              cfg.OwnerTag,
		OwnerValue:            cfg.OwnerValue,
		SourcePrincipalARN:    cfg.SourcePrincipalARN,
		AMIs:                  cfg.AMIs,
		Subnets:               cfg.Subnets,
		SecurityGroups:        cfg.SecurityGroups,
		AssetBucket:           cfg.AssetBucket,
		AssetPrefix:           cfg.AssetPrefix,
		IncludeWindows:        includeWindows,
		IncludeVPCCNI:         includeVPCCNI,
		VPCCNIFamily:          fam,
		IncludeAMILifecycle:   cfg.IncludeAMILifecycle,
		IncludeAMIPerformance: cfg.IncludeAMIPerformance || includeWindows,
		IncludeObservability:  cfg.IncludeObservability,
	}
	if reader != nil {
		if err := awsiam.DiscoverOptionsFromCluster(ctx, reader, &opts); err != nil {
			return awsiam.Options{}, err
		}
	}
	return opts, nil
}

// RenderIAMBundle renders the complete AWS IAM policy, role, and instance-profile
// bundle for a platform tuple.
func RenderIAMBundle(ctx context.Context, reader client.Reader, cfg IAMConfig) (*awsiam.Bundle, error) {
	opts, err := BuildIAMOptions(ctx, reader, cfg)
	if err != nil {
		return nil, err
	}
	return awsiam.Render(opts)
}

// ApplyIAMBundle renders and idempotently reconciles the platform tuple's AWS IAM
// policies, roles, instance profile, and optional Kubernetes AWSClusterRoleIdentity.
func ApplyIAMBundle(ctx context.Context, api awsiam.API, writer client.Client, reader client.Reader, cfg IAMConfig, dryRun bool) (*awsiam.ReconcileReport, *awsiam.Bundle, error) {
	bundle, err := RenderIAMBundle(ctx, reader, cfg)
	if err != nil {
		return nil, nil, err
	}
	report, err := awsiam.Reconcile(ctx, api, bundle, dryRun)
	if err != nil {
		return report, bundle, err
	}
	if cfg.ClusterRoleIdentity != "" && reader != nil && len(bundle.Roles) > 0 {
		capaRoleARN := bundle.Roles[0].ARN
		ns := cfg.AllowedNamespace
		if ns == "" {
			ns = "cloud-provisioning"
		}
		updated, err := awsiam.ReconcileAWSClusterRoleIdentity(ctx, writer, reader, cfg.ClusterRoleIdentity, capaRoleARN, cfg.SourceIdentityName, ns, dryRun)
		if err != nil {
			return report, bundle, err
		}
		if updated {
			report.ClusterIdentityUpdated = cfg.ClusterRoleIdentity
			report.Changes = append(report.Changes, fmt.Sprintf("reconciled AWSClusterRoleIdentity %s -> %s", cfg.ClusterRoleIdentity, capaRoleARN))
		}
	}
	return report, bundle, nil
}
