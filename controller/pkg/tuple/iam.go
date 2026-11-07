package tuple

import (
	"context"
	"fmt"
	"strings"

	attachmentaws "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	awsiam "github.com/appmana/cloud-provisioning/controller/pkg/platform/aws/iam"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IAMConfig supplies cluster and AWS identifiers to render or reconcile the
// tuple's AWS IAM policies, roles, and instance profile.
type IAMConfig struct {
	Tuple                 string
	Scope                 string
	BundleID              string
	MigrateFromBundles    []string
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
	CreateVPC             bool
	EC2                   attachmentaws.API
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
		BundleID:              cfg.BundleID,
		MigrateFromBundles:    cfg.MigrateFromBundles,
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
	if cfg.CreateVPC && cfg.EC2 != nil && !dryRun {
		clusterName := bundle.ClusterName
		if clusterName == "" {
			clusterName = cfg.ClusterName
		}
		vpcRep, err := joinaws.ProvisionIsolatedVPC(ctx, cfg.EC2, clusterName)
		if err != nil {
			return report, bundle, err
		}
		report.VPCID = vpcRep.VPCID
		report.SubnetID = vpcRep.SubnetID
		report.SecurityGroupID = vpcRep.SecurityGroupID
		report.Changes = append(report.Changes, vpcRep.Changes...)
		if writer != nil && reader != nil {
			ns := cfg.AllowedNamespace
			if ns == "" {
				ns = "cloud-provisioning"
			}
			if err := reconcileAWSProviderConfigSecret(ctx, writer, reader, ns, vpcRep.VPCID, vpcRep.SubnetID, vpcRep.SecurityGroupID); err != nil {
				return report, bundle, err
			}
		}
	}
	return report, bundle, nil
}

// CleanupIAMBundle safely tears down all tagged IAM policies, roles, and instance
// profile in the rendered bundle after verifying ownership tags on every resource,
// plus any controller-provisioned EC2 Security Group or dedicated VPC when an EC2
// client is supplied.
func CleanupIAMBundle(ctx context.Context, api awsiam.API, reader client.Reader, cfg IAMConfig, dryRun bool) (*awsiam.CleanupReport, *awsiam.Bundle, error) {
	bundle, err := RenderIAMBundle(ctx, reader, cfg)
	if err != nil {
		return nil, nil, err
	}
	report, err := awsiam.Cleanup(ctx, api, bundle, dryRun)
	if err != nil {
		return report, bundle, err
	}
	if cfg.EC2 != nil && !dryRun {
		clusterName := bundle.ClusterName
		if clusterName == "" {
			clusterName = cfg.ClusterName
		}
		deletedNet, err := joinaws.CleanupIsolatedVPC(ctx, cfg.EC2, clusterName)
		if err != nil {
			return report, bundle, err
		}
		report.NetworkResourcesDeleted = append(report.NetworkResourcesDeleted, deletedNet...)
		for _, id := range deletedNet {
			report.Changes = append(report.Changes, fmt.Sprintf("deleted managed EC2 resource %s", id))
		}
	}
	return report, bundle, nil
}

func reconcileAWSProviderConfigSecret(ctx context.Context, writer client.Client, reader client.Reader, namespace, vpcID, subnetID, sgID string) error {
	sec := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: "aws-provider-config"}
	err := reader.Get(ctx, key, sec)
	if apierrors.IsNotFound(err) {
		sec = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "aws-provider-config", Namespace: namespace},
			Data: map[string][]byte{
				"vpc-id":             []byte(vpcID),
				"subnet-id":          []byte(subnetID),
				"security-group-ids": []byte(sgID),
			},
		}
		return writer.Create(ctx, sec)
	}
	if err != nil {
		return err
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	if vpcID != "" {
		sec.Data["vpc-id"] = []byte(vpcID)
	}
	if subnetID != "" {
		sec.Data["subnet-id"] = []byte(subnetID)
	}
	if sgID != "" {
		sec.Data["security-group-ids"] = []byte(sgID)
	}
	return writer.Update(ctx, sec)
}
