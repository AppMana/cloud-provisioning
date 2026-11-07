package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	attachmentaws "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ManagedByTagKey marks EC2 network resources created automatically by cloud-provisioning.
	ManagedByTagKey = "cloud-provisioning.appmana.com/managed-by"
	// ManagedByTagValue is the required ownership tag value for automatic cleanup.
	ManagedByTagValue = "cloud-provisioning"
	// ClusterTagKey records which CAPI Cluster owns an automatically provisioned network resource.
	ClusterTagKey = "cloud-provisioning.appmana.com/cluster"
	// ManagedSGAnnotation records the controller-managed Security Group ID on AWSCluster.
	ManagedSGAnnotation = "cloud-provisioning.appmana.com/managed-security-group"
	// DefaultIsolatedVPCCIDR is the CIDR used when provisioning a dedicated VPC via --create-vpc.
	DefaultIsolatedVPCCIDR = "172.29.0.0/24"
	// DefaultIsolatedSubnetCIDR is the public subnet CIDR inside DefaultIsolatedVPCCIDR.
	DefaultIsolatedSubnetCIDR = "172.29.0.0/24"
)

// ManagedSecurityGroupName returns the deterministic Security Group name for a CAPI cluster.
func ManagedSecurityGroupName(clusterName string) string {
	name := strings.TrimSpace(clusterName)
	if name == "" {
		name = "workers"
	}
	return "cloud-provisioning-" + name
}

type ec2Tag struct {
	Key   string
	Value string
}

func hasEC2Tag(tags []ec2Tag, key, value string) bool {
	for _, t := range tags {
		if t.Key == key && (value == "" || t.Value == value) {
			return true
		}
	}
	return false
}

func standardResourceTags(clusterName, resourceName string) []map[string]any {
	tags := []map[string]any{
		{"Key": ManagedByTagKey, "Value": ManagedByTagValue},
		{"Key": "sigs.k8s.io/cluster-api-provider-aws/managed", "Value": "true"},
	}
	if strings.TrimSpace(clusterName) != "" {
		tags = append(tags,
			map[string]any{"Key": ClusterTagKey, "Value": clusterName},
			map[string]any{"Key": "sigs.k8s.io/cluster-api-provider-aws/cluster/" + clusterName, "Value": "owned"},
		)
	}
	if strings.TrimSpace(resourceName) != "" {
		tags = append(tags, map[string]any{"Key": "Name", "Value": resourceName})
	}
	return tags
}

// EnsureMachineNetwork implements join.ClusterNetworkProvisioner.
// Because an external AWSCluster (cluster.x-k8s.io/managed-by: external) tells
// CAPA's AWSCluster controller to stand down, CAPA does not automatically pick
// a VPC, subnet, or security group. EnsureMachineNetwork:
//  1. Resolves the VPC and public subnet (from AWSCluster.spec.network,
//     aws-provider-config, a cluster-tagged VPC, or the region's default VPC)
//     and patches AWSCluster.spec.network and AWSMachine.spec.subnet.
//  2. Automatically creates and authorizes the cluster's WireGuard + pod-mesh
//     security group (cloud-provisioning-<cluster>) in that VPC when the
//     AWSMachine has no concrete security group ID, and sets
//     AWSMachine.spec.additionalSecurityGroups = [{id: sg-...}].
func (p Provider) EnsureMachineNetwork(ctx context.Context, c client.Client, reader client.Reader, cluster, awsMachine *unstructured.Unstructured) error {
	if awsMachine == nil || awsMachine.GetKind() != gvk.Kind || cluster == nil {
		return nil
	}
	if p.Reader == nil && reader != nil {
		p.Reader = reader
	}
	defaults := p.ConfigDefaults(ctx)
	spec, _ := awsMachine.Object["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		awsMachine.Object["spec"] = spec
	}
	arch := awsMachine.GetLabels()["cloud-provisioning.appmana.com/arch"]
	if defaults.AMIAMD64 != "" || defaults.AMIARM64 != "" {
		if org, _ := spec["imageLookupOrg"].(string); org == DefaultImageLookupOrg {
			instType, _ := spec["instanceType"].(string)
			isARM := arch == "arm64" || IsARM64InstanceType(instType)
			if isARM && defaults.AMIARM64 != "" {
				spec["ami"] = map[string]any{"id": defaults.AMIARM64}
				delete(spec, "imageLookupOrg")
				delete(spec, "imageLookupBaseOS")
				delete(spec, "imageLookupFormat")
			} else if !isARM && defaults.AMIAMD64 != "" {
				spec["ami"] = map[string]any{"id": defaults.AMIAMD64}
				delete(spec, "imageLookupOrg")
				delete(spec, "imageLookupBaseOS")
				delete(spec, "imageLookupFormat")
			}
		}
	}
	if err := PrepareAWSMachineSpec(spec, arch, defaults); err != nil {
		return err
	}

	infraKind, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "kind")
	infraName, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "name")
	if infraKind != awsClusterGVK.Kind || infraName == "" || reader == nil {
		return nil
	}
	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(awsClusterGVK)
	if err := reader.Get(ctx, client.ObjectKey{Namespace: cluster.GetNamespace(), Name: infraName}, awsCluster); err != nil {
		return fmt.Errorf("reading AWSCluster %q: %w", infraName, err)
	}
	region, _, _ := unstructured.NestedString(awsCluster.Object, "spec", "region")
	if region == "" {
		return fmt.Errorf("AWSCluster %q names no region", infraName)
	}
	if p.EC2 == nil {
		return nil
	}
	api, err := p.EC2(ctx, region)
	if err != nil {
		return err
	}

	clusterName := cluster.GetName()
	vpcID, _, _ := unstructured.NestedString(awsCluster.Object, "spec", "network", "vpc", "id")
	if vpcID == "" {
		vpcID = defaults.VPCID
	}
	clusterSubnets := extractClusterSubnetIDs(awsCluster)
	machineSubnetID, _, _ := unstructured.NestedString(awsMachine.Object, "spec", "subnet", "id")
	subnetID := machineSubnetID
	if subnetID == "" && len(clusterSubnets) > 0 {
		subnetID = clusterSubnets[0]
	}
	if subnetID == "" && defaults.SubnetID != "" {
		subnetID = defaults.SubnetID
	}

	existingSGIDs, filterSGName := inspectAdditionalSecurityGroups(awsMachine)
	if vpcID == "" || subnetID == "" {
		discVPC, discSubnet, discErr := DiscoverVPCAndSubnet(ctx, api, clusterName, vpcID, subnetID)
		if discErr != nil && len(existingSGIDs) == 0 {
			return discErr
		}
		if vpcID == "" {
			vpcID = discVPC
		}
		if subnetID == "" {
			subnetID = discSubnet
		}
	}

	if machineSubnetID == "" && subnetID != "" {
		spec["subnet"] = map[string]any{"id": subnetID}
	}

	var managedSGID string
	if len(existingSGIDs) == 0 {
		sgName := filterSGName
		if sgName == "" {
			sgName = ManagedSecurityGroupName(clusterName)
		}
		sgID, err := EnsureClusterSecurityGroup(ctx, api, vpcID, clusterName, sgName)
		if err != nil {
			return err
		}
		managedSGID = sgID
		spec["additionalSecurityGroups"] = []any{
			map[string]any{"id": sgID},
		}
	}

	if c != nil {
		origVPC, _, _ := unstructured.NestedString(awsCluster.Object, "spec", "network", "vpc", "id")
		needsClusterPatch := false
		patched := awsCluster.DeepCopy()
		if origVPC == "" && vpcID != "" {
			_ = unstructured.SetNestedField(patched.Object, vpcID, "spec", "network", "vpc", "id")
			needsClusterPatch = true
		}
		if len(clusterSubnets) == 0 && subnetID != "" {
			_ = unstructured.SetNestedSlice(patched.Object, []any{
				map[string]any{"id": subnetID, "resourceID": subnetID, "isPublic": true},
			}, "spec", "network", "subnets")
			needsClusterPatch = true
		}
		if managedSGID != "" {
			anns := patched.GetAnnotations()
			if anns == nil {
				anns = map[string]string{}
			}
			if anns[ManagedSGAnnotation] != managedSGID {
				anns[ManagedSGAnnotation] = managedSGID
				patched.SetAnnotations(anns)
				needsClusterPatch = true
			}
		}
		if needsClusterPatch {
			if err := c.Patch(ctx, patched, client.MergeFrom(awsCluster)); err != nil {
				return fmt.Errorf("updating AWSCluster %q network spec: %w", awsCluster.GetName(), err)
			}
		}
	}
	return nil
}

// CleanupClusterNetwork implements join.ClusterNetworkProvisioner.
// When the last ProvisionedNodeClaim in a Cluster finishes deleting its CAPI
// Machine and AWSMachine, CleanupClusterNetwork deletes the controller-managed
// Security Group (cloud-provisioning-<cluster>) if it carries the ownership tag
// cloud-provisioning.appmana.com/managed-by=cloud-provisioning and has no
// remaining ENIs attached.
func (p Provider) CleanupClusterNetwork(ctx context.Context, c client.Client, reader client.Reader, cluster *unstructured.Unstructured) error {
	if cluster == nil || reader == nil || p.EC2 == nil {
		return nil
	}
	infraKind, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "kind")
	infraName, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "name")
	if infraKind != awsClusterGVK.Kind || infraName == "" {
		return nil
	}
	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(awsClusterGVK)
	if err := reader.Get(ctx, client.ObjectKey{Namespace: cluster.GetNamespace(), Name: infraName}, awsCluster); err != nil {
		return client.IgnoreNotFound(err)
	}
	region, _, _ := unstructured.NestedString(awsCluster.Object, "spec", "region")
	if region == "" {
		return nil
	}
	api, err := p.EC2(ctx, region)
	if err != nil {
		return err
	}
	vpcID, _, _ := unstructured.NestedString(awsCluster.Object, "spec", "network", "vpc", "id")
	if err := CleanupClusterSecurityGroup(ctx, api, vpcID, cluster.GetName()); err != nil {
		return err
	}
	if c != nil && awsCluster.GetAnnotations()[ManagedSGAnnotation] != "" {
		patched := awsCluster.DeepCopy()
		anns := patched.GetAnnotations()
		delete(anns, ManagedSGAnnotation)
		patched.SetAnnotations(anns)
		_ = c.Patch(ctx, patched, client.MergeFrom(awsCluster))
	}
	return nil
}

func extractClusterSubnetIDs(awsCluster *unstructured.Unstructured) []string {
	var out []string
	specs, _, _ := unstructured.NestedSlice(awsCluster.Object, "spec", "network", "subnets")
	for _, raw := range specs {
		m, _ := raw.(map[string]any)
		id, _ := m["resourceID"].(string)
		if id == "" {
			id, _ = m["id"].(string)
		}
		if strings.TrimSpace(id) != "" {
			out = append(out, strings.TrimSpace(id))
		}
	}
	return out
}

func inspectAdditionalSecurityGroups(awsMachine *unstructured.Unstructured) (ids []string, filterGroupName string) {
	groups, _, _ := unstructured.NestedSlice(awsMachine.Object, "spec", "additionalSecurityGroups")
	for _, raw := range groups {
		ref, _ := raw.(map[string]any)
		if id, _ := ref["id"].(string); strings.TrimSpace(id) != "" {
			ids = append(ids, strings.TrimSpace(id))
			continue
		}
		filters, _ := ref["filters"].([]any)
		for _, f := range filters {
			fm, _ := f.(map[string]any)
			name, _ := fm["name"].(string)
			if name != "group-name" {
				continue
			}
			vals, _ := fm["values"].([]any)
			for _, v := range vals {
				if s, _ := v.(string); strings.TrimSpace(s) != "" && filterGroupName == "" {
					filterGroupName = strings.TrimSpace(s)
				}
			}
		}
	}
	return ids, filterGroupName
}

// DiscoverVPCAndSubnet discovers the target VPC and public Subnet in EC2 when
// they are not explicitly set on AWSCluster.spec.network:
//   - If subnetID is known and vpcID is empty, queries describe-subnets to read
//     the subnet's VpcId.
//   - If vpcID is empty, queries describe-vpcs and selects:
//     1. A VPC tagged for clusterName or managed by cloud-provisioning
//     2. The region's default VPC (IsDefault: true)
//     3. The sole available VPC in the region
//   - If subnetID is empty, queries describe-subnets in vpcID and selects:
//     1. A subnet tagged for clusterName or role=public
//     2. A public subnet with MapPublicIpOnLaunch=true
//     3. A default-for-AZ subnet or the first available subnet in the VPC
func DiscoverVPCAndSubnet(ctx context.Context, api attachmentaws.API, clusterName, vpcID, subnetID string) (string, string, error) {
	if vpcID == "" && subnetID != "" {
		raw, err := api.Call(ctx, "ec2", "describe-subnets", map[string]any{
			"SubnetIds": []string{subnetID},
		})
		if err == nil && len(raw) > 0 {
			var subResp struct {
				Subnets []struct {
					SubnetId string
					VpcId    string
				}
			}
			if json.Unmarshal(raw, &subResp) == nil && len(subResp.Subnets) > 0 {
				vpcID = subResp.Subnets[0].VpcId
			}
		}
	}

	if vpcID == "" {
		raw, err := api.Call(ctx, "ec2", "describe-vpcs", map[string]any{})
		if err != nil {
			return "", "", fmt.Errorf("describing VPCs: %w", err)
		}
		var vpcResp struct {
			Vpcs []struct {
				VpcId     string
				IsDefault bool
				State     string
				Tags      []ec2Tag
			}
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &vpcResp); err != nil {
				return "", "", fmt.Errorf("decoding describe-vpcs: %w", err)
			}
		}
		var available []struct {
			VpcId     string
			IsDefault bool
			State     string
			Tags      []ec2Tag
		}
		for _, v := range vpcResp.Vpcs {
			if v.VpcId != "" && (v.State == "" || v.State == "available") {
				available = append(available, v)
			}
		}
		clusterTag := "sigs.k8s.io/cluster-api-provider-aws/cluster/" + clusterName
		for _, v := range available {
			if clusterName != "" && (hasEC2Tag(v.Tags, ClusterTagKey, clusterName) || hasEC2Tag(v.Tags, clusterTag, "")) {
				vpcID = v.VpcId
				break
			}
		}
		if vpcID == "" {
			for _, v := range available {
				if hasEC2Tag(v.Tags, ManagedByTagKey, ManagedByTagValue) {
					vpcID = v.VpcId
					break
				}
			}
		}
		if vpcID == "" {
			for _, v := range available {
				if v.IsDefault {
					vpcID = v.VpcId
					break
				}
			}
		}
		if vpcID == "" && len(available) == 1 {
			vpcID = available[0].VpcId
		}
		if vpcID == "" && len(available) > 1 {
			return "", "", fmt.Errorf("multiple non-default VPCs found in AWS region and none is tagged for cluster %q; set spec.network.vpc.id on AWSCluster or run 'endpoint-controller iam apply --create-vpc'", clusterName)
		}
	}

	if subnetID == "" && vpcID != "" {
		raw, err := api.Call(ctx, "ec2", "describe-subnets", map[string]any{
			"Filters": []map[string]any{
				{"Name": "vpc-id", "Values": []string{vpcID}},
			},
		})
		if err != nil {
			return vpcID, "", fmt.Errorf("describing subnets in VPC %s: %w", vpcID, err)
		}
		var subResp struct {
			Subnets []struct {
				SubnetId            string
				VpcId               string
				AvailabilityZone    string
				MapPublicIpOnLaunch bool
				DefaultForAz        bool
				State               string
				Tags                []ec2Tag
			}
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &subResp); err != nil {
				return vpcID, "", fmt.Errorf("decoding describe-subnets: %w", err)
			}
		}
		var candidates []struct {
			SubnetId            string
			VpcId               string
			AvailabilityZone    string
			MapPublicIpOnLaunch bool
			DefaultForAz        bool
			State               string
			Tags                []ec2Tag
		}
		for _, s := range subResp.Subnets {
			if s.SubnetId == "" {
				continue
			}
			if s.VpcId != "" && s.VpcId != vpcID {
				continue
			}
			if s.State != "" && s.State != "available" {
				continue
			}
			candidates = append(candidates, s)
		}
		clusterTag := "sigs.k8s.io/cluster-api-provider-aws/cluster/" + clusterName
		for _, s := range candidates {
			if (clusterName != "" && (hasEC2Tag(s.Tags, ClusterTagKey, clusterName) || hasEC2Tag(s.Tags, clusterTag, ""))) ||
				hasEC2Tag(s.Tags, "sigs.k8s.io/cluster-api-provider-aws/role", "public") {
				subnetID = s.SubnetId
				break
			}
		}
		if subnetID == "" {
			for _, s := range candidates {
				if s.MapPublicIpOnLaunch {
					subnetID = s.SubnetId
					break
				}
			}
		}
		if subnetID == "" {
			for _, s := range candidates {
				if s.DefaultForAz {
					subnetID = s.SubnetId
					break
				}
			}
		}
		if subnetID == "" && len(candidates) > 0 {
			subnetID = candidates[0].SubnetId
		}
	}
	return vpcID, subnetID, nil
}

// EnsureClusterSecurityGroup finds or creates the cluster's WireGuard + pod-mesh
// Security Group in vpcID and ensures it authorizes:
//   - UDP 51820 from 0.0.0.0/0 (so on-premises nodes behind NAT can dial the cloud worker)
//   - All protocols (-1) from the security group itself (so pods and nodes in the
//     security group can communicate directly across the AWS VPC CNI)
func EnsureClusterSecurityGroup(ctx context.Context, api attachmentaws.API, vpcID, clusterName, sgName string) (string, error) {
	if strings.TrimSpace(sgName) == "" {
		sgName = ManagedSecurityGroupName(clusterName)
	}
	filters := []map[string]any{
		{"Name": "group-name", "Values": []string{sgName}},
	}
	if strings.TrimSpace(vpcID) != "" {
		filters = append(filters, map[string]any{"Name": "vpc-id", "Values": []string{vpcID}})
	}
	raw, err := api.Call(ctx, "ec2", "describe-security-groups", map[string]any{
		"Filters": filters,
	})
	if err != nil {
		return "", fmt.Errorf("describing security group %q: %w", sgName, err)
	}
	var descResp struct {
		SecurityGroups []struct {
			GroupId       string
			GroupName     string
			Tags          []ec2Tag
			IpPermissions []struct {
				IpProtocol       string
				FromPort         int
				ToPort           int
				IpRanges         []struct{ CidrIp string }
				UserIdGroupPairs []struct{ GroupId string }
			}
		}
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &descResp); err != nil {
			return "", fmt.Errorf("decoding describe-security-groups: %w", err)
		}
	}
	if len(descResp.SecurityGroups) > 0 && descResp.SecurityGroups[0].GroupId != "" {
		sg := descResp.SecurityGroups[0]
		hasSelfAll := false
		hasWireGuard := false
		for _, rule := range sg.IpPermissions {
			if rule.IpProtocol == "-1" {
				for _, pair := range rule.UserIdGroupPairs {
					if pair.GroupId == sg.GroupId {
						hasSelfAll = true
					}
				}
			}
			if rule.IpProtocol == "udp" && rule.FromPort <= 51820 && rule.ToPort >= 51820 {
				hasWireGuard = true
			}
		}
		if hasEC2Tag(sg.Tags, ManagedByTagKey, ManagedByTagValue) && (!hasSelfAll || !hasWireGuard) {
			var missingPerms []map[string]any
			if !hasWireGuard {
				missingPerms = append(missingPerms, map[string]any{
					"IpProtocol": "udp",
					"FromPort":   51820,
					"ToPort":     51820,
					"IpRanges":   []map[string]any{{"CidrIp": "0.0.0.0/0", "Description": "WireGuard tunnel from on-premises nodes"}},
				})
			}
			if !hasSelfAll {
				missingPerms = append(missingPerms, map[string]any{
					"IpProtocol":       "-1",
					"UserIdGroupPairs": []map[string]any{{"GroupId": sg.GroupId, "Description": "Pod-to-pod and node-to-node mesh traffic"}},
				})
			}
			if _, err := api.Call(ctx, "ec2", "authorize-security-group-ingress", map[string]any{
				"GroupId":       sg.GroupId,
				"IpPermissions": missingPerms,
			}); err != nil {
				return "", fmt.Errorf("authorizing ingress rules on security group %s: %w", sg.GroupId, err)
			}
		}
		return sg.GroupId, nil
	}

	createInput := map[string]any{
		"GroupName":   sgName,
		"Description": fmt.Sprintf("cloud-provisioning WireGuard tunnel and pod mesh for cluster %s", clusterName),
		"TagSpecifications": []map[string]any{
			{
				"ResourceType": "security-group",
				"Tags":         standardResourceTags(clusterName, sgName),
			},
		},
	}
	if strings.TrimSpace(vpcID) != "" {
		createInput["VpcId"] = vpcID
	}
	createRaw, err := api.Call(ctx, "ec2", "create-security-group", createInput)
	if err != nil {
		return "", fmt.Errorf("creating security group %q: %w", sgName, err)
	}
	var createResp struct {
		GroupId string
	}
	if err := json.Unmarshal(createRaw, &createResp); err != nil || createResp.GroupId == "" {
		return "", fmt.Errorf("decoding create-security-group response for %q: %w", sgName, err)
	}
	sgID := createResp.GroupId

	if _, err := api.Call(ctx, "ec2", "authorize-security-group-ingress", map[string]any{
		"GroupId": sgID,
		"IpPermissions": []map[string]any{
			{
				"IpProtocol": "udp",
				"FromPort":   51820,
				"ToPort":     51820,
				"IpRanges":   []map[string]any{{"CidrIp": "0.0.0.0/0", "Description": "WireGuard tunnel from on-premises nodes"}},
			},
			{
				"IpProtocol":       "-1",
				"UserIdGroupPairs": []map[string]any{{"GroupId": sgID, "Description": "Pod-to-pod and node-to-node mesh traffic"}},
			},
		},
	}); err != nil {
		return "", fmt.Errorf("authorizing ingress on security group %s: %w", sgID, err)
	}
	return sgID, nil
}

// CleanupClusterSecurityGroup deletes the controller-managed Security Group for
// clusterName when it carries cloud-provisioning.appmana.com/managed-by=cloud-provisioning
// and no ENIs reference it.
func CleanupClusterSecurityGroup(ctx context.Context, api attachmentaws.API, vpcID, clusterName string) error {
	sgName := ManagedSecurityGroupName(clusterName)
	filters := []map[string]any{
		{"Name": "group-name", "Values": []string{sgName}},
	}
	if strings.TrimSpace(vpcID) != "" {
		filters = append(filters, map[string]any{"Name": "vpc-id", "Values": []string{vpcID}})
	}
	raw, err := api.Call(ctx, "ec2", "describe-security-groups", map[string]any{
		"Filters": filters,
	})
	if err != nil || len(raw) == 0 {
		return err
	}
	var descResp struct {
		SecurityGroups []struct {
			GroupId   string
			GroupName string
			Tags      []ec2Tag
		}
	}
	if err := json.Unmarshal(raw, &descResp); err != nil {
		return err
	}
	for _, sg := range descResp.SecurityGroups {
		if sg.GroupId == "" || !hasEC2Tag(sg.Tags, ManagedByTagKey, ManagedByTagValue) {
			continue
		}
		if clusterName != "" && !hasEC2Tag(sg.Tags, ClusterTagKey, clusterName) {
			continue
		}
		niRaw, err := api.Call(ctx, "ec2", "describe-network-interfaces", map[string]any{
			"Filters": []map[string]any{
				{"Name": "group-id", "Values": []string{sg.GroupId}},
			},
		})
		if err == nil && len(niRaw) > 0 {
			var niResp struct {
				NetworkInterfaces []struct {
					NetworkInterfaceId string
				}
			}
			if json.Unmarshal(niRaw, &niResp) == nil && len(niResp.NetworkInterfaces) > 0 {
				continue
			}
		}
		if _, err := api.Call(ctx, "ec2", "delete-security-group", map[string]any{
			"GroupId": sg.GroupId,
		}); err != nil {
			return fmt.Errorf("deleting unused security group %s (%s): %w", sg.GroupId, sgName, err)
		}
	}
	return nil
}

// VPCProvisionReport summarizes the VPC, public Subnet, Internet Gateway, Route Table,
// and Security Group provisioned or discovered for a cluster.
type VPCProvisionReport struct {
	VPCID           string   `json:"vpcId"`
	SubnetID        string   `json:"subnetId"`
	InternetGateway string   `json:"internetGatewayId,omitempty"`
	RouteTableID    string   `json:"routeTableId,omitempty"`
	SecurityGroupID string   `json:"securityGroupId"`
	Changes         []string `json:"changes,omitempty"`
}

// ProvisionIsolatedVPC idempotently provisions a dedicated tagged VPC, public
// Subnet, Internet Gateway, Route Table, and WireGuard + pod-mesh Security Group
// for clusterName when an account lacks a default VPC or desires an isolated VPC.
func ProvisionIsolatedVPC(ctx context.Context, api attachmentaws.API, clusterName string) (*VPCProvisionReport, error) {
	if strings.TrimSpace(clusterName) == "" {
		clusterName = "workers"
	}
	resName := "cloud-provisioning-" + clusterName
	rep := &VPCProvisionReport{}

	// 1. Find or create VPC
	vpcRaw, err := api.Call(ctx, "ec2", "describe-vpcs", map[string]any{
		"Filters": []map[string]any{
			{"Name": "tag:" + ManagedByTagKey, "Values": []string{ManagedByTagValue}},
			{"Name": "tag:" + ClusterTagKey, "Values": []string{clusterName}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describing VPCs for %s: %w", clusterName, err)
	}
	var vpcDesc struct {
		Vpcs []struct{ VpcId string }
	}
	_ = json.Unmarshal(vpcRaw, &vpcDesc)
	if len(vpcDesc.Vpcs) > 0 && vpcDesc.Vpcs[0].VpcId != "" {
		rep.VPCID = vpcDesc.Vpcs[0].VpcId
	} else {
		createVPCRaw, err := api.Call(ctx, "ec2", "create-vpc", map[string]any{
			"CidrBlock": DefaultIsolatedVPCCIDR,
			"TagSpecifications": []map[string]any{
				{"ResourceType": "vpc", "Tags": standardResourceTags(clusterName, resName)},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("creating VPC for %s: %w", clusterName, err)
		}
		var createVPCResp struct {
			Vpc struct{ VpcId string }
		}
		if err := json.Unmarshal(createVPCRaw, &createVPCResp); err != nil || createVPCResp.Vpc.VpcId == "" {
			return nil, fmt.Errorf("decoding create-vpc response: %w", err)
		}
		rep.VPCID = createVPCResp.Vpc.VpcId
		_, _ = api.Call(ctx, "ec2", "modify-vpc-attribute", map[string]any{
			"VpcId":              rep.VPCID,
			"EnableDnsSupport":   map[string]any{"Value": true},
		})
		_, _ = api.Call(ctx, "ec2", "modify-vpc-attribute", map[string]any{
			"VpcId":              rep.VPCID,
			"EnableDnsHostnames": map[string]any{"Value": true},
		})
		rep.Changes = append(rep.Changes, fmt.Sprintf("created VPC %s (%s)", rep.VPCID, DefaultIsolatedVPCCIDR))
	}

	// 2. Find or create Internet Gateway and attach to VPC
	igwRaw, err := api.Call(ctx, "ec2", "describe-internet-gateways", map[string]any{
		"Filters": []map[string]any{
			{"Name": "tag:" + ManagedByTagKey, "Values": []string{ManagedByTagValue}},
			{"Name": "tag:" + ClusterTagKey, "Values": []string{clusterName}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describing internet gateways: %w", err)
	}
	var igwDesc struct {
		InternetGateways []struct {
			InternetGatewayId string
			Attachments       []struct{ VpcId string }
		}
	}
	_ = json.Unmarshal(igwRaw, &igwDesc)
	var attached bool
	if len(igwDesc.InternetGateways) > 0 && igwDesc.InternetGateways[0].InternetGatewayId != "" {
		rep.InternetGateway = igwDesc.InternetGateways[0].InternetGatewayId
		for _, a := range igwDesc.InternetGateways[0].Attachments {
			if a.VpcId == rep.VPCID {
				attached = true
			}
		}
	} else {
		createIGWRaw, err := api.Call(ctx, "ec2", "create-internet-gateway", map[string]any{
			"TagSpecifications": []map[string]any{
				{"ResourceType": "internet-gateway", "Tags": standardResourceTags(clusterName, resName)},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("creating internet gateway: %w", err)
		}
		var createIGWResp struct {
			InternetGateway struct{ InternetGatewayId string }
		}
		if err := json.Unmarshal(createIGWRaw, &createIGWResp); err != nil || createIGWResp.InternetGateway.InternetGatewayId == "" {
			return nil, fmt.Errorf("decoding create-internet-gateway: %w", err)
		}
		rep.InternetGateway = createIGWResp.InternetGateway.InternetGatewayId
		rep.Changes = append(rep.Changes, fmt.Sprintf("created InternetGateway %s", rep.InternetGateway))
	}
	if !attached {
		if _, err := api.Call(ctx, "ec2", "attach-internet-gateway", map[string]any{
			"InternetGatewayId": rep.InternetGateway,
			"VpcId":             rep.VPCID,
		}); err != nil {
			return nil, fmt.Errorf("attaching internet gateway %s to %s: %w", rep.InternetGateway, rep.VPCID, err)
		}
		rep.Changes = append(rep.Changes, fmt.Sprintf("attached InternetGateway %s to VPC %s", rep.InternetGateway, rep.VPCID))
	}

	// 3. Find or create public Subnet
	subRaw, err := api.Call(ctx, "ec2", "describe-subnets", map[string]any{
		"Filters": []map[string]any{
			{"Name": "vpc-id", "Values": []string{rep.VPCID}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describing subnets in %s: %w", rep.VPCID, err)
	}
	var subDesc struct {
		Subnets []struct{ SubnetId string }
	}
	_ = json.Unmarshal(subRaw, &subDesc)
	if len(subDesc.Subnets) > 0 && subDesc.Subnets[0].SubnetId != "" {
		rep.SubnetID = subDesc.Subnets[0].SubnetId
	} else {
		subTags := append(standardResourceTags(clusterName, resName+"-public"),
			map[string]any{"Key": "sigs.k8s.io/cluster-api-provider-aws/role", "Value": "public"},
		)
		createSubRaw, err := api.Call(ctx, "ec2", "create-subnet", map[string]any{
			"VpcId":     rep.VPCID,
			"CidrBlock": DefaultIsolatedSubnetCIDR,
			"TagSpecifications": []map[string]any{
				{"ResourceType": "subnet", "Tags": subTags},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("creating public subnet in %s: %w", rep.VPCID, err)
		}
		var createSubResp struct {
			Subnet struct{ SubnetId string }
		}
		if err := json.Unmarshal(createSubRaw, &createSubResp); err != nil || createSubResp.Subnet.SubnetId == "" {
			return nil, fmt.Errorf("decoding create-subnet: %w", err)
		}
		rep.SubnetID = createSubResp.Subnet.SubnetId
		_, _ = api.Call(ctx, "ec2", "modify-subnet-attribute", map[string]any{
			"SubnetId":            rep.SubnetID,
			"MapPublicIpOnLaunch": map[string]any{"Value": true},
		})
		rep.Changes = append(rep.Changes, fmt.Sprintf("created public Subnet %s (%s)", rep.SubnetID, DefaultIsolatedSubnetCIDR))
	}

	// 4. Find or create public Route Table and default route to Internet Gateway
	rtRaw, err := api.Call(ctx, "ec2", "describe-route-tables", map[string]any{
		"Filters": []map[string]any{
			{"Name": "vpc-id", "Values": []string{rep.VPCID}},
			{"Name": "tag:" + ManagedByTagKey, "Values": []string{ManagedByTagValue}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describing route tables in %s: %w", rep.VPCID, err)
	}
	var rtDesc struct {
		RouteTables []struct {
			RouteTableId string
			Associations []struct{ SubnetId string }
			Routes       []struct{ DestinationCidrBlock, GatewayId string }
		}
	}
	_ = json.Unmarshal(rtRaw, &rtDesc)
	var hasDefaultRoute, hasAssociation bool
	if len(rtDesc.RouteTables) > 0 && rtDesc.RouteTables[0].RouteTableId != "" {
		rep.RouteTableID = rtDesc.RouteTables[0].RouteTableId
		for _, r := range rtDesc.RouteTables[0].Routes {
			if r.DestinationCidrBlock == "0.0.0.0/0" && r.GatewayId == rep.InternetGateway {
				hasDefaultRoute = true
			}
		}
		for _, a := range rtDesc.RouteTables[0].Associations {
			if a.SubnetId == rep.SubnetID {
				hasAssociation = true
			}
		}
	} else {
		createRTRaw, err := api.Call(ctx, "ec2", "create-route-table", map[string]any{
			"VpcId": rep.VPCID,
			"TagSpecifications": []map[string]any{
				{"ResourceType": "route-table", "Tags": standardResourceTags(clusterName, resName+"-rtb")},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("creating route table in %s: %w", rep.VPCID, err)
		}
		var createRTResp struct {
			RouteTable struct{ RouteTableId string }
		}
		if err := json.Unmarshal(createRTRaw, &createRTResp); err != nil || createRTResp.RouteTable.RouteTableId == "" {
			return nil, fmt.Errorf("decoding create-route-table: %w", err)
		}
		rep.RouteTableID = createRTResp.RouteTable.RouteTableId
		rep.Changes = append(rep.Changes, fmt.Sprintf("created RouteTable %s", rep.RouteTableID))
	}
	if !hasDefaultRoute {
		if _, err := api.Call(ctx, "ec2", "create-route", map[string]any{
			"RouteTableId":         rep.RouteTableID,
			"DestinationCidrBlock": "0.0.0.0/0",
			"GatewayId":            rep.InternetGateway,
		}); err != nil {
			return nil, fmt.Errorf("creating default route in %s: %w", rep.RouteTableID, err)
		}
		rep.Changes = append(rep.Changes, fmt.Sprintf("created route 0.0.0.0/0 -> %s in %s", rep.InternetGateway, rep.RouteTableID))
	}
	if !hasAssociation {
		if _, err := api.Call(ctx, "ec2", "associate-route-table", map[string]any{
			"RouteTableId": rep.RouteTableID,
			"SubnetId":     rep.SubnetID,
		}); err != nil {
			return nil, fmt.Errorf("associating route table %s with subnet %s: %w", rep.RouteTableID, rep.SubnetID, err)
		}
		rep.Changes = append(rep.Changes, fmt.Sprintf("associated RouteTable %s with Subnet %s", rep.RouteTableID, rep.SubnetID))
	}

	// 5. Ensure cluster Security Group
	sgID, err := EnsureClusterSecurityGroup(ctx, api, rep.VPCID, clusterName, ManagedSecurityGroupName(clusterName))
	if err != nil {
		return nil, err
	}
	rep.SecurityGroupID = sgID
	return rep, nil
}

// CleanupIsolatedVPC tears down any controller-managed Security Group and any
// dedicated VPC/Subnet/RouteTable/InternetGateway tagged
// cloud-provisioning.appmana.com/managed-by=cloud-provisioning for clusterName.
func CleanupIsolatedVPC(ctx context.Context, api attachmentaws.API, clusterName string) ([]string, error) {
	if strings.TrimSpace(clusterName) == "" {
		clusterName = "workers"
	}
	var deleted []string

	// Clean up any managed Security Group first (whether in default VPC or dedicated VPC)
	if err := CleanupClusterSecurityGroup(ctx, api, "", clusterName); err != nil {
		return deleted, err
	}

	vpcRaw, err := api.Call(ctx, "ec2", "describe-vpcs", map[string]any{
		"Filters": []map[string]any{
			{"Name": "tag:" + ManagedByTagKey, "Values": []string{ManagedByTagValue}},
			{"Name": "tag:" + ClusterTagKey, "Values": []string{clusterName}},
		},
	})
	if err != nil || len(vpcRaw) == 0 {
		return deleted, err
	}
	var vpcDesc struct {
		Vpcs []struct {
			VpcId string
			Tags  []ec2Tag
		}
	}
	if err := json.Unmarshal(vpcRaw, &vpcDesc); err != nil {
		return deleted, err
	}
	for _, vpc := range vpcDesc.Vpcs {
		if vpc.VpcId == "" || !hasEC2Tag(vpc.Tags, ManagedByTagKey, ManagedByTagValue) {
			continue
		}
		if err := CleanupClusterSecurityGroup(ctx, api, vpc.VpcId, clusterName); err != nil {
			return deleted, err
		}

		// Delete tagged route tables
		rtRaw, _ := api.Call(ctx, "ec2", "describe-route-tables", map[string]any{
			"Filters": []map[string]any{
				{"Name": "vpc-id", "Values": []string{vpc.VpcId}},
				{"Name": "tag:" + ManagedByTagKey, "Values": []string{ManagedByTagValue}},
			},
		})
		var rtDesc struct {
			RouteTables []struct {
				RouteTableId string
				Associations []struct {
					RouteTableAssociationId string
					Main                    bool
				}
			}
		}
		_ = json.Unmarshal(rtRaw, &rtDesc)
		for _, rt := range rtDesc.RouteTables {
			for _, assoc := range rt.Associations {
				if !assoc.Main && assoc.RouteTableAssociationId != "" {
					_, _ = api.Call(ctx, "ec2", "disassociate-route-table", map[string]any{
						"AssociationId": assoc.RouteTableAssociationId,
					})
				}
			}
			if _, err := api.Call(ctx, "ec2", "delete-route-table", map[string]any{
				"RouteTableId": rt.RouteTableId,
			}); err == nil {
				deleted = append(deleted, rt.RouteTableId)
			}
		}

		// Delete tagged subnets
		subRaw, _ := api.Call(ctx, "ec2", "describe-subnets", map[string]any{
			"Filters": []map[string]any{
				{"Name": "vpc-id", "Values": []string{vpc.VpcId}},
				{"Name": "tag:" + ManagedByTagKey, "Values": []string{ManagedByTagValue}},
			},
		})
		var subDesc struct {
			Subnets []struct{ SubnetId string }
		}
		_ = json.Unmarshal(subRaw, &subDesc)
		for _, sub := range subDesc.Subnets {
			if _, err := api.Call(ctx, "ec2", "delete-subnet", map[string]any{
				"SubnetId": sub.SubnetId,
			}); err == nil {
				deleted = append(deleted, sub.SubnetId)
			}
		}

		// Detach and delete tagged internet gateways
		igwRaw, _ := api.Call(ctx, "ec2", "describe-internet-gateways", map[string]any{
			"Filters": []map[string]any{
				{"Name": "tag:" + ManagedByTagKey, "Values": []string{ManagedByTagValue}},
				{"Name": "tag:" + ClusterTagKey, "Values": []string{clusterName}},
			},
		})
		var igwDesc struct {
			InternetGateways []struct {
				InternetGatewayId string
				Attachments       []struct{ VpcId string }
			}
		}
		_ = json.Unmarshal(igwRaw, &igwDesc)
		for _, igw := range igwDesc.InternetGateways {
			for _, a := range igw.Attachments {
				if a.VpcId == vpc.VpcId {
					_, _ = api.Call(ctx, "ec2", "detach-internet-gateway", map[string]any{
						"InternetGatewayId": igw.InternetGatewayId,
						"VpcId":             vpc.VpcId,
					})
				}
			}
			if _, err := api.Call(ctx, "ec2", "delete-internet-gateway", map[string]any{
				"InternetGatewayId": igw.InternetGatewayId,
			}); err == nil {
				deleted = append(deleted, igw.InternetGatewayId)
			}
		}

		if _, err := api.Call(ctx, "ec2", "delete-vpc", map[string]any{
			"VpcId": vpc.VpcId,
		}); err != nil {
			return deleted, fmt.Errorf("deleting VPC %s: %w", vpc.VpcId, err)
		}
		deleted = append(deleted, vpc.VpcId)
	}
	return deleted, nil
}

// ValidateClass implements join.ClassValidator. A VPC CNI node's pods take
// addresses on the node's own interface and talk to pods on other VPC CNI
// nodes directly, so the security groups the machine gets must admit each
// other: some group among them allows all traffic from a group among them.
// Without that, pods on one node reach pods on another only by accident of
// the groups' other rules, and the claim is refused before any machine
// exists.
func (p Provider) ValidateClass(ctx context.Context, c client.Reader, awsMachine *unstructured.Unstructured, class platform.Class) error {
	if class.CNI != VPCCNI || awsMachine.GetKind() != gvk.Kind {
		return nil
	}
	var ids []string
	groups, _, _ := unstructured.NestedSlice(awsMachine.Object, "spec", "additionalSecurityGroups")
	for _, raw := range groups {
		ref, _ := raw.(map[string]any)
		id, _ := ref["id"].(string)
		if id == "" {
			return fmt.Errorf("the AWS VPC CNI needs the machine's security groups named by ID to check they admit each other; %v names one by filter", ref)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return fmt.Errorf("the AWS VPC CNI needs the machine's security groups named (spec.additionalSecurityGroups) so pods on different nodes can be checked to reach each other")
	}
	region, err := p.region(ctx, c, awsMachine)
	if err != nil {
		return err
	}
	if p.EC2 == nil {
		return fmt.Errorf("no EC2 API to check the machine's security groups with")
	}
	api, err := p.EC2(ctx, region)
	if err != nil {
		return err
	}
	raw, err := api.Call(ctx, "ec2", "describe-security-groups", map[string]any{"GroupIds": ids})
	if err != nil {
		return fmt.Errorf("describing security groups %v: %w", ids, err)
	}
	var response struct {
		SecurityGroups []struct {
			GroupId       string
			IpPermissions []struct {
				IpProtocol       string
				UserIdGroupPairs []struct{ GroupId string }
			}
		}
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return err
	}
	for _, group := range response.SecurityGroups {
		for _, rule := range group.IpPermissions {
			if rule.IpProtocol != "-1" {
				continue
			}
			for _, pair := range rule.UserIdGroupPairs {
				if slices.Contains(ids, pair.GroupId) {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("security groups %s admit no traffic from each other; AWS VPC CNI pods on different nodes need one of them to allow all protocols from one of them (for example an ingress rule on the group from itself)", strings.Join(ids, ", "))
}

// region is the region of the machine's AWSCluster.
func (Provider) region(ctx context.Context, c client.Reader, awsMachine *unstructured.Unstructured) (string, error) {
	clusterName := awsMachine.GetLabels()[clusterNameLabel]
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: awsMachine.GetNamespace(), Name: clusterName}, cluster); err != nil {
		return "", fmt.Errorf("reading Cluster %q: %w", clusterName, err)
	}
	name, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "name")
	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(awsClusterGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: awsMachine.GetNamespace(), Name: name}, awsCluster); err != nil {
		return "", fmt.Errorf("reading AWSCluster %q: %w", name, err)
	}
	region, _, _ := unstructured.NestedString(awsCluster.Object, "spec", "region")
	if region == "" {
		return "", fmt.Errorf("AWSCluster %q names no region", name)
	}
	return region, nil
}
