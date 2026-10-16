package awspod

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type ec2 struct {
	eni       map[string]any
	calls     int
	fail      bool
	token     string
	afterRead func()
}

func (e *ec2) Call(_ context.Context, service, op string, input map[string]any) (json.RawMessage, error) {
	if service != "ec2" || op != "describe-network-interfaces" {
		return nil, fmt.Errorf("unexpected mutation")
	}
	if input["Filters"].([]map[string]any)[0]["Values"].([]string)[0] != "i-123" {
		return nil, fmt.Errorf("wrong instance")
	}
	e.calls++
	if e.afterRead != nil {
		e.afterRead()
	}
	if e.fail {
		return nil, fmt.Errorf("EC2 unavailable")
	}
	return json.Marshal(map[string]any{"NetworkInterfaces": []any{e.eni}, "NextToken": e.token})
}
func fixture(t *testing.T) (*Observer, *unstructured.Unstructured, *ec2) {
	t.Helper()
	m := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cluster.x-k8s.io/v1beta2", "kind": "Machine", "metadata": map[string]any{"name": "worker", "namespace": "test", "uid": "machine-uid"}, "spec": map[string]any{"clusterName": "cluster", "providerID": "aws:///us-west-2a/i-123"}, "status": map[string]any{"nodeRef": map[string]any{"name": "worker", "uid": "node-uid"}}}}
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node-uid", Labels: map[string]string{NodeLabel: NodeValue, "kubernetes.io/os": "linux", "cloud-provisioning.appmana.com/role": "cloud-worker"}}, Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-123"}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(m, n).Build()
	api := &ec2{eni: map[string]any{"NetworkInterfaceId": "eni-1", "OwnerId": "123", "VpcId": "vpc-1", "SubnetId": "subnet-1", "Status": "in-use", "Attachment": map[string]any{"InstanceId": "i-123", "Status": "attached"}, "TagSet": []any{map[string]any{"Key": "cluster", "Value": "test"}}, "PrivateIpAddresses": []any{map[string]any{"PrivateIpAddress": "172.29.0.10", "Primary": true}, map[string]any{"PrivateIpAddress": "172.29.0.11", "Primary": false}}, "Ipv6Prefixes": []any{map[string]any{"Ipv6Prefix": "2001:db8:1::/80"}}}}
	return &Observer{Reader: c, API: api, Config: Config{Account: "123", Region: "us-west-2", VPC: "vpc-1", Subnets: []string{"subnet-1"}, Namespace: "test", Cluster: "cluster", OwnerTag: "cluster", OwnerValue: "test", Family: "ipv4", PodRanges: []string{"172.29.0.0/24"}}}, m, api
}
func TestObservedAllocationFamiliesAndRelease(t *testing.T) {
	for _, family := range []string{"ipv4", "ipv6"} {
		t.Run(family, func(t *testing.T) {
			o, m, api := fixture(t)
			o.Config.Family = family
			want := "172.29.0.11/32"
			if family == "ipv6" {
				o.Config.PodRanges = []string{"2001:db8:1::/64"}
				want = "2001:db8:1::/80"
			}
			got, err := o.PrefixesFor(context.Background(), m, "worker")
			if err != nil || len(got) != 1 || got[0].String() != want {
				t.Fatalf("%v %v", got, err)
			}
			delete(api.eni, "PrivateIpAddresses")
			delete(api.eni, "Ipv6Prefixes")
			got, err = o.PrefixesFor(context.Background(), m, "worker")
			if err != nil || len(got) != 0 {
				t.Fatalf("released allocation retained: %v %v", got, err)
			}
		})
	}
}
func TestRejectUnverifiedAllocations(t *testing.T) {
	for _, kind := range []string{"machine-uid", "node-uid", "provider", "owner", "vpc", "subnet", "tag", "instance", "detached", "range", "dual-stack", "broad-v6", "v4-prefix", "duplicate", "api", "pagination"} {
		t.Run(kind, func(t *testing.T) {
			o, m, api := fixture(t)
			switch kind {
			case "machine-uid":
				m.SetUID("replacement")
			case "node-uid", "provider":
				n := &corev1.Node{}
				if err := o.Reader.Get(context.Background(), client.ObjectKey{Name: "worker"}, n); err != nil {
					t.Fatal(err)
				}
				if kind == "node-uid" {
					n.UID = "replacement"
				} else {
					n.Spec.ProviderID = "aws:///us-west-2a/i-other"
				}
				if err := o.Reader.(client.Client).Update(context.Background(), n); err != nil {
					t.Fatal(err)
				}
			case "owner":
				api.eni["OwnerId"] = "foreign"
			case "vpc":
				api.eni["VpcId"] = "foreign"
			case "subnet":
				api.eni["SubnetId"] = "foreign"
			case "tag":
				delete(api.eni, "TagSet")
			case "instance":
				api.eni["Attachment"] = map[string]any{"InstanceId": "i-other", "Status": "attached"}
			case "detached":
				api.eni["Status"] = "available"
			case "range":
				o.Config.PodRanges = []string{"10.0.0.0/24"}
			case "dual-stack":
				o.Config.Family = "dual"
			case "broad-v6":
				o.Config.Family = "ipv6"
				o.Config.PodRanges = []string{"2001:db8:1::/64"}
				api.eni["Ipv6Prefixes"] = []any{map[string]any{"Ipv6Prefix": "2001:db8:1::/64"}}
			case "v4-prefix":
				api.eni["Ipv4Prefixes"] = []any{map[string]any{"Ipv4Prefix": "172.29.0.16/28"}}
			case "duplicate":
				api.eni["PrivateIpAddresses"] = []any{map[string]any{"PrivateIpAddress": "172.29.0.11"}, map[string]any{"PrivateIpAddress": "172.29.0.11"}}
			case "api":
				api.fail = true
			case "pagination":
				api.token = "repeat"
			}
			if got, err := o.PrefixesFor(context.Background(), m, "worker"); err == nil {
				t.Fatalf("accepted %s: %v", kind, got)
			}
		})
	}
}

func TestNameOnlyCAPIBinding(t *testing.T) {
	for _, fault := range []string{"", "missing-reverse", "foreign-machine", "foreign-cluster", "not-running", "old-api", "node-replaced-during-read", "provider-changed-during-read"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			o, m, api := fixture(t)
			c := o.Reader.(client.Client)
			unstructured.RemoveNestedField(m.Object, "status", "nodeRef", "uid")
			_ = unstructured.SetNestedField(m.Object, "Running", "status", "phase")
			if fault == "not-running" {
				_ = unstructured.SetNestedField(m.Object, "Provisioned", "status", "phase")
			}
			if fault == "old-api" {
				m.SetAPIVersion("cluster.x-k8s.io/v1beta1")
			}
			n := &corev1.Node{}
			if err := c.Get(ctx, client.ObjectKey{Name: "worker"}, n); err != nil {
				t.Fatal(err)
			}
			n.Annotations = map[string]string{"cluster.x-k8s.io/machine": "worker", "cluster.x-k8s.io/cluster-name": "cluster", "cluster.x-k8s.io/cluster-namespace": "test"}
			switch fault {
			case "missing-reverse":
				n.Annotations = nil
			case "foreign-machine":
				n.Annotations["cluster.x-k8s.io/machine"] = "other"
			case "foreign-cluster":
				n.Annotations["cluster.x-k8s.io/cluster-namespace"] = "other"
			}
			c = fake.NewClientBuilder().WithScheme(c.Scheme()).WithObjects(m, n).Build()
			o.Reader = c
			if fault == "node-replaced-during-read" || fault == "provider-changed-during-read" {
				api.afterRead = func() {
					current := &corev1.Node{}
					if err := c.Get(ctx, client.ObjectKey{Name: "worker"}, current); err != nil {
						t.Fatal(err)
					}
					if fault == "node-replaced-during-read" {
						current.UID = "replacement"
					} else {
						current.Spec.ProviderID = "aws:///us-west-2a/i-other"
					}
					if err := c.Update(ctx, current); err != nil {
						t.Fatal(err)
					}
				}
			}
			got, err := o.PrefixesFor(ctx, m, "worker")
			if fault == "" {
				if err != nil || len(got) != 1 {
					t.Fatalf("name-only association rejected: %v %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("accepted %s: %v", fault, got)
			}
		})
	}
}

func TestPrimaryENIAddressIsNativeButNeverPodOwned(t *testing.T) {
	o, machine, _ := fixture(t)
	allocation, err := o.AllocationsFor(context.Background(), machine, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(allocation.PodPrefixes) != 1 || allocation.PodPrefixes[0].String() != "172.29.0.11/32" {
		t.Fatalf("primary leaked into pod ownership: %+v", allocation)
	}
	if len(allocation.NativePrefixes) != 2 || allocation.NativePrefixes[0].String() != "172.29.0.10/32" {
		t.Fatalf("native primary missing: %+v", allocation)
	}
}

func TestIPv6ENIAddressesAreNativeButNeverPodOwned(t *testing.T) {
	for _, family := range []string{"ipv4", "ipv6"} {
		t.Run(family, func(t *testing.T) {
			o, machine, api := fixture(t)
			o.Config.Family = family
			if family == "ipv6" {
				o.Config.PodRanges = []string{"2001:db8:1::/64"}
			}
			api.eni["Ipv6Addresses"] = []any{
				map[string]any{"Ipv6Address": "2001:db8:1:0:1234::1", "IsPrimaryIpv6": true},
				map[string]any{"Ipv6Address": "2001:db8:1:0:1234::2", "IsPrimaryIpv6": false},
			}
			allocation, err := o.AllocationsFor(context.Background(), machine, "worker")
			if err != nil {
				t.Fatal(err)
			}
			if len(allocation.PodPrefixes) != 1 {
				t.Fatalf("host addresses leaked into pod ownership: %+v", allocation)
			}
			native := map[string]bool{}
			for _, p := range allocation.NativePrefixes {
				native[p.String()] = true
			}
			for _, host := range []string{"2001:db8:1:0:1234::1/128", "2001:db8:1:0:1234::2/128"} {
				if !native[host] {
					t.Errorf("native IPv6 host missing: %s", host)
				}
			}
			delete(api.eni, "Ipv6Addresses")
			released, err := o.AllocationsFor(context.Background(), machine, "worker")
			if err != nil {
				t.Fatal(err)
			}
			if len(released.NativePrefixes) != 2 {
				t.Fatalf("released host retained: %+v", released)
			}
		})
	}
}

func TestRejectInvalidNativeIPv6Host(t *testing.T) {
	for _, address := range []string{"", "garbage", "172.29.0.10", "::ffff:172.29.0.10", "::", "::1", "ff02::1", "fe80::1"} {
		t.Run(address, func(t *testing.T) {
			o, machine, api := fixture(t)
			api.eni["Ipv6Addresses"] = []any{map[string]any{"Ipv6Address": address}}
			if _, err := o.AllocationsFor(context.Background(), machine, "worker"); err == nil {
				t.Fatal("accepted invalid native IPv6 host")
			}
		})
	}
}

func TestWindowsSecondaryIPAllocationAndWithdrawal(t *testing.T) {
	for _, scenario := range []string{"valid", "secondary-eni", "missing-index", "ipv6", "unknown-os", "foreign-owner"} {
		t.Run(scenario, func(t *testing.T) {
			o, m, api := fixture(t)
			ctx := context.Background()
			node := &corev1.Node{}
			if err := o.Reader.Get(ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
				t.Fatal(err)
			}
			node.Labels[corev1.LabelOSStable] = "windows"
			attachment := api.eni["Attachment"].(map[string]any)
			attachment["DeviceIndex"] = 0
			switch scenario {
			case "secondary-eni":
				attachment["DeviceIndex"] = 1
			case "missing-index":
				delete(attachment, "DeviceIndex")
			case "ipv6":
				o.Config.Family = "ipv6"
				o.Config.PodRanges = []string{"2001:db8:1::/64"}
			case "unknown-os":
				node.Labels[corev1.LabelOSStable] = "other"
			case "foreign-owner":
				api.eni["OwnerId"] = "foreign"
			}
			if err := o.Reader.(client.Client).Update(ctx, node); err != nil {
				t.Fatal(err)
			}
			allocation, err := o.AllocationsFor(ctx, m, "worker")
			if scenario != "valid" {
				if err == nil {
					t.Fatal("unqualified Windows allocation accepted")
				}
				return
			}
			if err != nil || len(allocation.PodPrefixes) != 1 || allocation.PodPrefixes[0].String() != "172.29.0.11/32" || len(allocation.NativePrefixes) != 2 {
				t.Fatalf("Windows primary ENI allocation: %+v %v", allocation, err)
			}
			api.eni["PrivateIpAddresses"] = []any{map[string]any{"PrivateIpAddress": "172.29.0.10", "Primary": true}}
			allocation, err = o.AllocationsFor(ctx, m, "worker")
			if err != nil || len(allocation.PodPrefixes) != 0 || len(allocation.NativePrefixes) != 1 {
				t.Fatalf("Windows released secondary IP retained: %+v %v", allocation, err)
			}
		})
	}
}
