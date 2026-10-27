package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	attachmentaws "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type groupsAPI struct {
	groups string
	asked  map[string]any
}

func (a *groupsAPI) Call(_ context.Context, service, operation string, input map[string]any) (json.RawMessage, error) {
	if service != "ec2" || operation != "describe-security-groups" {
		return nil, fmt.Errorf("unexpected %s %s", service, operation)
	}
	a.asked = input
	return json.RawMessage(a.groups), nil
}

func machineWithGroups(ids ...string) *unstructured.Unstructured {
	var groups []any
	for _, id := range ids {
		groups = append(groups, map[string]any{"id": id})
	}
	m := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"additionalSecurityGroups": groups}}}
	m.SetGroupVersionKind(gvk)
	m.SetNamespace("default")
	m.SetLabels(map[string]string{clusterNameLabel: "appmana"})
	return m
}

// Pods on different VPC CNI nodes talk directly, from addresses on each
// node's interface, so the nodes' security groups must admit each other.
// A claim whose groups do not is refused before any machine exists.
func TestVPCCNINodesNeedSecurityGroupsThatAdmitEachOther(t *testing.T) {
	native := platform.Class{OS: "linux", Arch: "amd64", CNI: VPCCNI}
	selfRule := `{"SecurityGroups":[{"GroupId":"sg-1","IpPermissions":[
		{"IpProtocol":"udp","FromPort":51820,"ToPort":51820,"IpRanges":[{"CidrIp":"0.0.0.0/0"}]},
		{"IpProtocol":"-1","UserIdGroupPairs":[{"GroupId":"sg-1"}]}]}]}`
	wireGuardOnly := `{"SecurityGroups":[{"GroupId":"sg-1","IpPermissions":[
		{"IpProtocol":"udp","FromPort":51820,"ToPort":51820,"IpRanges":[{"CidrIp":"0.0.0.0/0"}]}]}]}`
	for groups, ok := range map[string]bool{selfRule: true, wireGuardOnly: false} {
		api := &groupsAPI{groups: groups}
		p := Provider{EC2: func(_ context.Context, region string) (attachmentaws.API, error) {
			if region != "us-west-2" {
				t.Fatalf("region %q", region)
			}
			return api, nil
		}}
		err := p.ValidateClass(context.Background(), newReader(t, "us-west-2"), machineWithGroups("sg-1"), native)
		if (err == nil) != ok {
			t.Errorf("groups %s: %v", groups, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "sg-1")) {
			t.Errorf("the refusal does not name the groups: %v", err)
		}
	}
	// A tunnel-class node's pods reach each other through the tunnel.
	p := Provider{EC2: func(context.Context, string) (attachmentaws.API, error) {
		t.Fatal("EC2 asked about a tunnel-class node")
		return nil, nil
	}}
	if err := p.ValidateClass(context.Background(), newReader(t, "us-west-2"), machineWithGroups("sg-1"), platform.Class{OS: "linux", CNI: platform.SiteCNI}); err != nil {
		t.Fatal(err)
	}
	// A machine with no groups named cannot be checked, and is refused.
	if err := (Provider{EC2: func(context.Context, string) (attachmentaws.API, error) { return &groupsAPI{}, nil }}).ValidateClass(
		context.Background(), newReader(t, "us-west-2"), machineWithGroups(), native); err == nil {
		t.Fatal("a VPC CNI machine with no security groups was admitted")
	}
}

// newReader holds Cluster appmana and its AWSCluster in region.
func newReader(t *testing.T, region string) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(clusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(awsClusterGVK, &unstructured.Unstructured{})
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetNamespace("default")
	cluster.SetName("appmana")
	_ = unstructured.SetNestedField(cluster.Object, "AWSCluster", "spec", "infrastructureRef", "kind")
	_ = unstructured.SetNestedField(cluster.Object, "appmana-aws", "spec", "infrastructureRef", "name")
	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(awsClusterGVK)
	awsCluster.SetNamespace("default")
	awsCluster.SetName("appmana-aws")
	_ = unstructured.SetNestedField(awsCluster.Object, region, "spec", "region")
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, awsCluster).Build()
}
