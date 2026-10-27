package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
