package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"

	attachmentaws "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni/awspod"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	machineGVK    = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}
	clusterGVK    = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}
	awsClusterGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSCluster"}
)

// OwnerTag is the tag CAPA puts, with value "owned", on every instance it
// creates and that instance's primary network interface
// (cluster-api-provider-aws v2 infrav1.ClusterTagKey).
func OwnerTag(cluster string) string {
	return "sigs.k8s.io/cluster-api-provider-aws/cluster/" + cluster
}

// ClusterScopes resolves the EC2 scope of a Machine's pod allocations from
// its Cluster's AWSCluster, so nothing about the VPC is restated as
// configuration.
type ClusterScopes struct {
	Reader client.Reader
	// Family is the VPC CNI's pod address family, ipv4 or ipv6.
	Family string
	// API is an EC2 API in a region.
	API func(ctx context.Context, region string) (attachmentaws.API, error)
	// Account is the account the controller's credentials act in, asked of
	// a region.
	Account func(ctx context.Context, region string) (string, error)
}

var _ awspod.ScopeSource = ClusterScopes{}

func (s ClusterScopes) Scope(ctx context.Context, machine *unstructured.Unstructured) (awspod.Config, attachmentaws.API, error) {
	clusterName, _, _ := unstructured.NestedString(machine.Object, "spec", "clusterName")
	if clusterName == "" {
		return awspod.Config{}, nil, fmt.Errorf("Machine names no Cluster")
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: machine.GetNamespace(), Name: clusterName}, cluster); err != nil {
		return awspod.Config{}, nil, fmt.Errorf("reading Cluster %s: %w", clusterName, err)
	}
	kind, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "kind")
	name, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "name")
	if kind != awsClusterGVK.Kind || name == "" {
		return awspod.Config{}, nil, fmt.Errorf("Cluster %s is not an AWSCluster's", clusterName)
	}
	infra := &unstructured.Unstructured{}
	infra.SetGroupVersionKind(awsClusterGVK)
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: machine.GetNamespace(), Name: name}, infra); err != nil {
		return awspod.Config{}, nil, fmt.Errorf("reading AWSCluster %s: %w", name, err)
	}
	region, _, _ := unstructured.NestedString(infra.Object, "spec", "region")
	vpc, _, _ := unstructured.NestedString(infra.Object, "spec", "network", "vpc", "id")
	subnetSpecs, _, _ := unstructured.NestedSlice(infra.Object, "spec", "network", "subnets")
	var subnets []string
	for _, raw := range subnetSpecs {
		spec, _ := raw.(map[string]any)
		id, _ := spec["resourceID"].(string)
		if id == "" {
			id, _ = spec["id"].(string)
		}
		if id != "" {
			subnets = append(subnets, id)
		}
	}
	if region == "" || vpc == "" || len(subnets) == 0 {
		return awspod.Config{}, nil, fmt.Errorf("AWSCluster %s names no region, VPC or subnets", name)
	}
	api, err := s.API(ctx, region)
	if err != nil {
		return awspod.Config{}, nil, err
	}
	ranges, err := subnetRanges(ctx, api, vpc, subnets, s.Family)
	if err != nil {
		return awspod.Config{}, nil, err
	}
	account, err := s.Account(ctx, region)
	if err != nil {
		return awspod.Config{}, nil, fmt.Errorf("reading the AWS account: %w", err)
	}
	return awspod.Config{
		Account: account, Region: region, VPC: vpc, Subnets: subnets,
		Namespace: machine.GetNamespace(), Cluster: clusterName,
		OwnerTag: OwnerTag(clusterName), OwnerValue: "owned",
		Family: s.Family, PodRanges: ranges,
	}, api, nil
}

// subnetRanges are the ranges the VPC CNI allocates pod addresses from in
// these subnets: each subnet's IPv4 range, or its one associated IPv6
// range, which must hold delegated /80 prefixes.
func subnetRanges(ctx context.Context, api attachmentaws.API, vpc string, subnets []string, family string) ([]string, error) {
	raw, err := api.Call(ctx, "ec2", "describe-subnets", map[string]any{"SubnetIds": subnets})
	if err != nil {
		return nil, fmt.Errorf("describing subnets %v: %w", subnets, err)
	}
	var response struct {
		Subnets []struct {
			SubnetId, VpcId, CidrBlock  string
			Ipv6CidrBlockAssociationSet []struct {
				Ipv6CidrBlock      string
				Ipv6CidrBlockState struct{ State string }
			}
		}
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	if len(response.Subnets) != len(subnets) {
		return nil, fmt.Errorf("EC2 described %d of subnets %v", len(response.Subnets), subnets)
	}
	var ranges []string
	for _, subnet := range response.Subnets {
		if subnet.VpcId != vpc {
			return nil, fmt.Errorf("subnet %s is in %s, not the AWSCluster's %s", subnet.SubnetId, subnet.VpcId, vpc)
		}
		switch family {
		case "ipv4":
			p, err := netip.ParsePrefix(subnet.CidrBlock)
			if err != nil || !p.Addr().Is4() {
				return nil, fmt.Errorf("subnet %s has no IPv4 range", subnet.SubnetId)
			}
			ranges = append(ranges, p.Masked().String())
		case "ipv6":
			var associated []netip.Prefix
			for _, a := range subnet.Ipv6CidrBlockAssociationSet {
				if a.Ipv6CidrBlockState.State != "associated" {
					continue
				}
				p, err := netip.ParsePrefix(a.Ipv6CidrBlock)
				if err != nil || !p.Addr().Is6() {
					return nil, fmt.Errorf("subnet %s has an invalid IPv6 range", subnet.SubnetId)
				}
				associated = append(associated, p.Masked())
			}
			if len(associated) != 1 || associated[0].Bits() > 80 {
				return nil, fmt.Errorf("subnet %s needs exactly one associated IPv6 range able to hold /80 prefixes", subnet.SubnetId)
			}
			ranges = append(ranges, associated[0].String())
		default:
			return nil, fmt.Errorf("VPC CNI family %q is neither ipv4 nor ipv6", family)
		}
	}
	return ranges, nil
}
