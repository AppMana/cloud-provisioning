package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	attachmentaws "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni/awspod"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type subnetsAPI struct {
	asked  []string
	region string
	vpc    string
}

func (a *subnetsAPI) Call(_ context.Context, service, operation string, input map[string]any) (json.RawMessage, error) {
	if service != "ec2" || operation != "describe-subnets" {
		return nil, fmt.Errorf("unexpected %s %s", service, operation)
	}
	for _, id := range input["SubnetIds"].([]string) {
		a.asked = append(a.asked, id)
	}
	return json.RawMessage(`{"Subnets":[{"SubnetId":"subnet-1","VpcId":"` + a.vpc + `","CidrBlock":"172.29.0.0/24",
		"Ipv6CidrBlockAssociationSet":[{"Ipv6CidrBlock":"2001:db8:1::/64","Ipv6CidrBlockState":{"State":"associated"}}]}]}`), nil
}

func object(gvk schema.GroupVersionKind, namespace, name string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: fields}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func scopeFixture(t *testing.T, family string, awsCluster map[string]any) (ClusterScopes, *unstructured.Unstructured, *subnetsAPI) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, gvk := range []schema.GroupVersionKind{machineGVK, clusterGVK, awsClusterGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	machine := object(machineGVK, "cloud-provisioning", "worker-1", map[string]any{"spec": map[string]any{"clusterName": "site"}})
	cluster := object(clusterGVK, "cloud-provisioning", "site", map[string]any{"spec": map[string]any{
		"infrastructureRef": map[string]any{"apiGroup": "infrastructure.cluster.x-k8s.io", "kind": "AWSCluster", "name": "site-aws"}}})
	infra := object(awsClusterGVK, "cloud-provisioning", "site-aws", awsCluster)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine, cluster, infra).Build()
	api := &subnetsAPI{vpc: "vpc-1"}
	return ClusterScopes{
		Reader: c, Family: family,
		API: func(_ context.Context, region string) (attachmentaws.API, error) {
			api.region = region
			return api, nil
		},
		Account: func(context.Context, string) (string, error) { return "123456789012", nil },
	}, machine, api
}

func externalNetwork() map[string]any {
	return map[string]any{"spec": map[string]any{"region": "us-west-2", "network": map[string]any{
		"vpc":     map[string]any{"id": "vpc-1"},
		"subnets": []any{map[string]any{"id": "subnet-1"}},
	}}}
}

// The observer's scope comes from the Machine's own infrastructure: the
// AWSCluster's region, VPC and subnets, the subnets' ranges from EC2, the
// account from STS, and CAPA's ownership tag, which CAPA puts on every
// instance's primary ENI. Nothing is restated as configuration.
func TestTheObservationScopeComesFromTheMachinesAWSCluster(t *testing.T) {
	for family, ranges := range map[string][]string{"ipv4": {"172.29.0.0/24"}, "ipv6": {"2001:db8:1::/64"}} {
		scopes, machine, api := scopeFixture(t, family, externalNetwork())
		cfg, got, err := scopes.Scope(context.Background(), machine)
		if err != nil {
			t.Fatal(err)
		}
		want := awspod.Config{Account: "123456789012", Region: "us-west-2", VPC: "vpc-1", Subnets: []string{"subnet-1"},
			Namespace: "cloud-provisioning", Cluster: "site", OwnerTag: "sigs.k8s.io/cluster-api-provider-aws/cluster/site",
			OwnerValue: "owned", Family: family, PodRanges: ranges}
		if !reflect.DeepEqual(cfg, want) {
			t.Fatalf("%s scope = %+v, want %+v", family, cfg, want)
		}
		if got != api || api.region != "us-west-2" || !reflect.DeepEqual(api.asked, []string{"subnet-1"}) {
			t.Fatalf("EC2 asked in %q about %v", api.region, api.asked)
		}
	}
}

// An AWSCluster that names no subnets, or subnets in another VPC, has no
// scope to observe pods in.
func TestAnIncompleteAWSClusterHasNoScope(t *testing.T) {
	noSubnets := externalNetwork()
	unstructured.RemoveNestedField(noSubnets, "spec", "network", "subnets")
	scopes, machine, _ := scopeFixture(t, "ipv4", noSubnets)
	if _, _, err := scopes.Scope(context.Background(), machine); err == nil {
		t.Fatal("an AWSCluster without subnets was given a scope")
	}
	scopes, machine, api := scopeFixture(t, "ipv4", externalNetwork())
	api.vpc = "vpc-other"
	if _, _, err := scopes.Scope(context.Background(), machine); err == nil {
		t.Fatal("a subnet outside the AWSCluster's VPC was accepted")
	}
}
