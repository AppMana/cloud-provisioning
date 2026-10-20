package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni/awspod"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAWSReleasedHostAllocationIsWithdrawn(t *testing.T) {
	m := &unstructured.Unstructured{}
	m.SetName("worker")
	m.SetAnnotations(map[string]string{join.WireGuardAddrAnnotation: "10.100.0.128/24"})
	s := &corev1.Secret{Data: map[string][]byte{tunnel.PeerAllowedIPsPrefix + "worker": []byte("10.100.0.128/32,172.29.0.11/32")}}
	for _, prefixes := range [][]netip.Prefix{nil, {netip.MustParsePrefix("172.29.0.12/32")}} {
		got, err := awsAllowedIPs(m, s, prefixes)
		if err != nil || strings.Contains(got, "172.29.0.11") || !strings.Contains(got, "10.100.0.128/32") {
			t.Fatalf("%q %v", got, err)
		}
	}
	s.Data[tunnel.SitePodCIDRsPrefix+"site"] = []byte("172.29.0.0/24")
	if _, err := awsAllowedIPs(m, s, []netip.Prefix{netip.MustParsePrefix("172.29.0.12/32")}); err == nil {
		t.Fatal("AWS stole site pod allocation")
	}
	delete(s.Data, tunnel.SitePodCIDRsPrefix+"site")
	s.Data[tunnel.PeerAllowedIPsPrefix+"other"] = []byte("172.29.0.12/32")
	if _, err := awsAllowedIPs(m, s, []netip.Prefix{netip.MustParsePrefix("172.29.0.12/32")}); err == nil {
		t.Fatal("duplicate ownership accepted")
	}
}
func TestAWSNodeDoesNotReceiveCalicoAddresses(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cloud", Labels: map[string]string{awspod.NodeLabel: awspod.NodeValue}}}
	r, c := dualStackReconciler(t, "", n)
	if err := r.ensureCNINodeAddress(context.Background(), n.Name, []string{"10.100.0.128"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(n), n); err != nil {
		t.Fatal(err)
	}
	if len(n.Annotations) != 0 {
		t.Fatalf("Calico mutated AWS node: %v", n.Annotations)
	}
	if _, err := r.publishRemotePodCIDRs(context.Background(), &unstructured.Unstructured{}, n.Name); err == nil {
		t.Fatal("AWS node fell back to Calico allocations")
	}
}

// Native CNI ownership conflicts must be reported without modifying the Node;
// these fake-client checks do not prove what CNI or BGP sessions run on a guest.
func TestAWSNodeRejectsRetainedCalicoBGPAddresses(t *testing.T) {
	for _, key := range []string{calicoIPv4Annotation, calicoIPv6Annotation} {
		for _, value := range []string{"10.100.0.128/32", "fd00:10:100::128/128", "invalid"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				ctx := context.Background()
				n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cloud", Labels: map[string]string{awspod.NodeLabel: awspod.NodeValue}, Annotations: map[string]string{key: value, "example.com/owner": "preserve"}}}
				r, c := dualStackReconciler(t, "", n)
				if err := c.Get(ctx, client.ObjectKeyFromObject(n), n); err != nil {
					t.Fatal(err)
				}
				before := n.DeepCopy()
				err := r.ensureCNINodeAddress(ctx, n.Name, []string{"10.100.0.128", "fd00:10:100::128"}, "test/claim")
				if err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("conflicting Calico ownership not reported: %v", err)
				}
				if err := c.Get(ctx, client.ObjectKeyFromObject(n), n); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, n) {
					t.Fatal("conflicting CNI configuration was modified")
				}
			})
		}
	}
}

type allocationAPI struct {
	address string
	fail    bool
}

func (a *allocationAPI) Call(_ context.Context, service, operation string, _ map[string]any) (json.RawMessage, error) {
	if service != "ec2" || operation != "describe-network-interfaces" || a.fail {
		return nil, fmt.Errorf("observation unavailable")
	}
	addresses := []any{}
	if a.address != "" {
		addresses = append(addresses, map[string]any{"PrivateIpAddress": a.address, "Primary": false})
	}
	return json.Marshal(map[string]any{"NetworkInterfaces": []any{map[string]any{"NetworkInterfaceId": "eni-1", "OwnerId": "123", "VpcId": "vpc-1", "SubnetId": "subnet-1", "Status": "in-use", "Attachment": map[string]any{"InstanceId": "i-123", "Status": "attached"}, "TagSet": []any{map[string]any{"Key": "run", "Value": "test"}}, "PrivateIpAddresses": addresses}}})
}
func TestAWSObservationPublishesAndWithdrawsWithoutCalicoFallback(t *testing.T) {
	ctx := context.Background()
	m := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cluster.x-k8s.io/v1beta2", "kind": "Machine", "metadata": map[string]any{"name": "worker", "namespace": "cloud-provisioning", "uid": "machine-uid"}, "spec": map[string]any{"clusterName": "cluster", "providerID": "aws:///us-west-2a/i-123"}, "status": map[string]any{"nodeRef": map[string]any{"name": "worker", "uid": "node-uid"}}}}
	m.SetAnnotations(map[string]string{join.WireGuardAddrAnnotation: "10.100.0.128/24"})
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node-uid", Labels: map[string]string{awspod.NodeLabel: awspod.NodeValue, cloudWorkerRoleLabel: cloudWorkerRoleValue, "kubernetes.io/os": "linux"}}, Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-123"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "peers", Namespace: "cloud-provisioning"}, Data: map[string][]byte{tunnel.PeerPublicKeyPrefix + "worker": []byte("worker-key"), tunnel.PeerAllowedIPsPrefix + "worker": []byte("10.100.0.128/32")}}
	r, c := dualStackReconciler(t, "", m, n, secret)
	// This test focuses on the publication transaction; NAT is independently
	// checked by the CNI package and must be qualified by the native campaign.
	r.network = cni.Network{Name: cni.KubeRouter, Encapsulation: cni.Native}
	api := &allocationAPI{address: "172.29.0.11"}
	r.awsPodObserver = &awspod.Observer{Reader: c, API: api, Config: awspod.Config{Account: "123", Region: "us-west-2", VPC: "vpc-1", Subnets: []string{"subnet-1"}, Namespace: "cloud-provisioning", Cluster: "cluster", OwnerTag: "run", OwnerValue: "test", Family: "ipv4", PodRanges: []string{"172.29.0.0/24"}}}
	read := func() string {
		t.Helper()
		if err := c.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
			t.Fatal(err)
		}
		return string(secret.Data[tunnel.PeerAllowedIPsPrefix+"worker"])
	}
	if ok, err := r.publishRemotePodCIDRs(ctx, m, n.Name); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}
	before := read()
	nativeBefore := string(secret.Data[tunnel.PeerNativeNetworkPrefix+"worker"])
	if !strings.Contains(nativeBefore, "aws:123:us-west-2:vpc-1") || !strings.Contains(nativeBefore, "172.29.0.11/32") {
		t.Fatalf("native network not published with allocation: %s", nativeBefore)
	}

	if !strings.Contains(before, "172.29.0.11/32") {
		t.Fatal("AWS allocation not published")
	}
	api.fail = true
	if _, err := r.publishRemotePodCIDRs(ctx, m, n.Name); err == nil {
		t.Fatal("API failure hidden")
	}
	if read() != before || string(secret.Data[tunnel.PeerNativeNetworkPrefix+"worker"]) != nativeBefore {
		t.Fatal("API failure withdrew known-good state")
	}
	api.fail = false
	api.address = ""
	if ok, err := r.publishRemotePodCIDRs(ctx, m, n.Name); err != nil || !ok {
		t.Fatalf("withdraw: %v %v", ok, err)
	}
	if got := read(); got != "10.100.0.128/32" {
		t.Fatalf("released IP retained: %s", got)
	}
	if strings.Contains(string(secret.Data[tunnel.PeerNativeNetworkPrefix+"worker"]), "172.29.0.11") {
		t.Fatal("released native allocation retained")
	}

}
