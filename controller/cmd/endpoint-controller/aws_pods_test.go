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
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	platformaws "github.com/appmana/cloud-provisioning/controller/pkg/platform/aws"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAWSReleasedHostAllocationIsWithdrawn(t *testing.T) {
	m := &unstructured.Unstructured{}
	m.SetName("worker")
	m.SetAnnotations(map[string]string{join.WireGuardAddrAnnotation: "10.100.0.128/24"})
	s := &corev1.Secret{Data: map[string][]byte{tunnel.PeerAllowedIPsPrefix + "worker": []byte("10.100.0.128/32,172.29.0.11/32")}}
	for _, prefixes := range [][]netip.Prefix{nil, {netip.MustParsePrefix("172.29.0.12/32")}} {
		got, err := nativeAllowedIPs(m, s, prefixes)
		if err != nil || strings.Contains(got, "172.29.0.11") || !strings.Contains(got, "10.100.0.128/32") {
			t.Fatalf("%q %v", got, err)
		}
	}
	s.Data[tunnel.SitePodCIDRsPrefix+"site"] = []byte("172.29.0.0/24")
	if _, err := nativeAllowedIPs(m, s, []netip.Prefix{netip.MustParsePrefix("172.29.0.12/32")}); err == nil {
		t.Fatal("AWS stole site pod allocation")
	}
	delete(s.Data, tunnel.SitePodCIDRsPrefix+"site")
	s.Data[tunnel.PeerAllowedIPsPrefix+"other"] = []byte("172.29.0.12/32")
	if _, err := nativeAllowedIPs(m, s, []netip.Prefix{netip.MustParsePrefix("172.29.0.12/32")}); err == nil {
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
	r.nativeObservers = map[string]platform.Observer{awspod.NodeValue: platformaws.Observer{Observer: &awspod.Observer{Reader: c, Scopes: awspod.StaticScope{API: api, Config: awspod.Config{Account: "123", Region: "us-west-2", VPC: "vpc-1", Subnets: []string{"subnet-1"}, Namespace: "cloud-provisioning", Cluster: "cluster", OwnerTag: "run", OwnerValue: "test", Family: "ipv4", PodRanges: []string{"172.29.0.0/24"}}}}}}
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

func TestAWSSameVPCMultiWorkerNativeNetworkProjection(t *testing.T) {
	ctx := context.Background()
	const (
		keySite = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA="
		keyW1   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB="
		keyW2   = "ccccccccccccccccccccccccccccccccccccccccccC="
	)
	m1 := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cluster.x-k8s.io/v1beta2",
		"kind":       "Machine",
		"metadata": map[string]any{
			"name":      "worker-1",
			"namespace": "cloud-provisioning",
			"uid":       "m1-uid",
		},
	}}
	m1.SetAnnotations(map[string]string{join.WireGuardAddrAnnotation: "10.100.0.128/24"})
	m2 := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cluster.x-k8s.io/v1beta2",
		"kind":       "Machine",
		"metadata": map[string]any{
			"name":      "worker-2",
			"namespace": "cloud-provisioning",
			"uid":       "m2-uid",
		},
		"spec": map[string]any{
			"clusterName": "cluster",
			"providerID":  "aws:///us-west-2a/i-123",
		},
		"status": map[string]any{
			"addresses": []any{
				map[string]any{"type": "ExternalIP", "address": "198.51.100.2"},
				map[string]any{"type": "InternalIP", "address": "172.29.0.70"},
			},
		},
	}}
	m2.SetAnnotations(map[string]string{join.WireGuardAddrAnnotation: "10.100.0.129/24"})

	w1Native, err := json.Marshal(tunnel.NativeNetwork{
		PublicKey: keyW1,
		ID:        "aws:123:us-west-2:vpc-1",
		Prefixes:  []string{"172.29.0.11/32", "172.29.0.92/32"},
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "peers", Namespace: "cloud-provisioning", UID: "mesh-uid", ResourceVersion: "1"},
		Data: map[string][]byte{
			tunnel.NodePublicKeyPrefix + "site":         []byte(keySite),
			tunnel.NodeTunnelAddressPrefix + "site":     []byte("10.100.0.1/24"),
			tunnel.NodeAddressesPrefix + "site":         []byte("172.21.0.10"),
			tunnel.NodePodCIDRsPrefix + "site":          []byte("10.244.1.0/24"),
			tunnel.PeerPublicKeyPrefix + "worker-1":     []byte(keyW1),
			tunnel.PeerEndpointPrefix + "worker-1":      []byte("198.51.100.1:51820"),
			tunnel.PeerAllowedIPsPrefix + "worker-1":    []byte("10.100.0.128/32,198.51.100.1/32,172.29.0.92/32,172.29.0.11/32"),
			tunnel.PeerRouteHostsPrefix + "worker-1":    []byte("10.100.0.128,198.51.100.1,172.29.0.92"),
			tunnel.PeerNativeNetworkPrefix + "worker-1": w1Native,
			tunnel.PeerPublicKeyPrefix + "worker-2":     []byte(keyW2),
			tunnel.PeerAllowedIPsPrefix + "worker-2":    []byte("10.100.0.129/32"),
			tunnel.PeerRouteHostsPrefix + "worker-2":    []byte("10.100.0.129"),
		},
	}

	r, c := dualStackReconciler(t, "", m1, m2, secret)
	r.secretKey = tunnel.PeerEndpointPrefix
	r.port = "51820"
	r.dialerDaemonSetName = "wg-dialer"
	r.dialerCloudDaemonSetName = "wg-dialer-cloud"
	r.network = cni.Network{Name: cni.KubeRouter, Encapsulation: cni.Native}
	r.receivePreparation = &receivePreparationPolicy{
		NativeCIDRs: []netip.Prefix{netip.MustParsePrefix("172.21.0.0/24")},
		Ports:       []int{51821, 51822},
	}
	api := &allocationAPI{address: "172.29.0.70"}
	r.nativeObservers = map[string]platform.Observer{
		awspod.NodeValue: platformaws.Observer{
			Observer: &awspod.Observer{
				Reader: c,
				Scopes: awspod.StaticScope{
					API: api,
					Config: awspod.Config{
						Account:    "123",
						Region:     "us-west-2",
						VPC:        "vpc-1",
						Subnets:    []string{"subnet-1"},
						Namespace:  "cloud-provisioning",
						Cluster:    "cluster",
						OwnerTag:   "run",
						OwnerValue: "test",
						Family:     "ipv4",
						PodRanges:  []string{"172.29.0.0/24"},
					},
				},
			},
		},
	}

	// Step 1: Reconcile worker-2 before its Node exists.
	// InternalIP (172.29.0.70) must NOT be published into peer-allowed-ips-worker-2
	// before peer-native-network-worker-2 is verified, so worker-1 never records
	// 172.29.0.70/32 as a tunnel destination.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "cloud-provisioning", Name: "worker-2"}}); err != nil {
		t.Fatalf("pre-node reconcile: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		t.Fatal(err)
	}
	if got := string(secret.Data[tunnel.PeerAllowedIPsPrefix+"worker-2"]); strings.Contains(got, "172.29.0.70") {
		t.Fatalf("InternalIP published before native network verification: %s", got)
	}
	if got := string(secret.Data[tunnel.PeerRouteHostsPrefix+"worker-2"]); strings.Contains(got, "172.29.0.70") {
		t.Fatalf("InternalIP route host published before native network verification: %s", got)
	}
	adoption := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "cloud-provisioning", Name: tunnel.AdoptionSecretName("worker-2")}, adoption); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(adoption.Data[tunnel.CloudPeersKey]), "172.29.0.92/32") {
		t.Fatalf("expected bootstrap adoption list before native verification to include worker-1 IP: %s", adoption.Data[tunnel.CloudPeersKey])
	}

	// Step 2: Node registers with AWS VPC CNI label and becomes Ready.
	n2 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-2",
			UID:  "n2-uid",
			Labels: map[string]string{
				awspod.NodeLabel:     awspod.NodeValue,
				cloudWorkerRoleLabel: cloudWorkerRoleValue,
				"kubernetes.io/os":   "linux",
			},
		},
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-123"},
		Status: corev1.NodeStatus{
			NodeInfo:   corev1.NodeSystemInfo{BootID: "boot-2"},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	if err := c.Create(ctx, n2); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(m2), m2); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedMap(m2.Object, map[string]any{"name": "worker-2", "uid": "n2-uid"}, "status", "nodeRef")
	if err := c.Update(ctx, m2); err != nil {
		t.Fatal(err)
	}

	// Reconcile now publishes peer-native-network-worker-2 AND updates worker-2's
	// peers.json to project out worker-1's same-VPC underlay addresses before
	// staging receive-prepare.json.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "cloud-provisioning", Name: "worker-2"}}); err != nil {
		t.Fatalf("post-node reconcile: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		t.Fatal(err)
	}
	if got := string(secret.Data[tunnel.PeerAllowedIPsPrefix+"worker-2"]); !strings.Contains(got, "172.29.0.70/32") {
		t.Fatalf("InternalIP not published with native network: %s", got)
	}
	if got := string(secret.Data[tunnel.PeerRouteHostsPrefix+"worker-2"]); !strings.Contains(got, "172.29.0.70") {
		t.Fatalf("InternalIP route host not published with native network: %s", got)
	}
	if len(secret.Data[tunnel.PeerNativeNetworkPrefix+"worker-2"]) == 0 {
		t.Fatal("peer-native-network-worker-2 not published")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "cloud-provisioning", Name: tunnel.AdoptionSecretName("worker-2")}, adoption); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(adoption.Data[tunnel.CloudPeersKey]), "172.29.0.92") {
		t.Fatalf("same-VPC neighbor native IP retained in worker-2 adoption peers.json: %s", adoption.Data[tunnel.CloudPeersKey])
	}
	if len(adoption.Data[tunnel.ReceivePrepareKey]) != 0 {
		t.Fatal("receive-prepare.json staged before dialer acknowledged projected peers.json")
	}
}
