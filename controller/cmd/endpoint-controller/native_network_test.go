package main

import (
	"context"
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
)

func nativeNetworkFixture(t *testing.T, labels map[string]string) (*meshReconciler, *unstructured.Unstructured) {
	t.Helper()
	m := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cluster.x-k8s.io/v1beta2", "kind": "Machine", "metadata": map[string]any{"name": "worker", "namespace": "cloud-provisioning"}}}
	m.SetAnnotations(map[string]string{join.WireGuardAddrAnnotation: "10.100.0.128/24"})
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: labels}, Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-123"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "peers", Namespace: "cloud-provisioning"}, Data: map[string][]byte{tunnel.PeerAllowedIPsPrefix + "worker": []byte("10.100.0.128/32")}}
	r, _ := dualStackReconciler(t, "", m, n, secret)
	r.network = cni.Network{Name: cni.KubeRouter, Encapsulation: cni.Native}
	return r, m
}

// A node registered on a provider-native network nobody observes cannot be
// given routes: its pod addresses are unknown, so this is an error rather
// than a silent fallback to the site CNI's blocks.
func TestANodeOnAnUnobservedNativeNetworkFails(t *testing.T) {
	r, m := nativeNetworkFixture(t, map[string]string{platform.CNILabel: awspod.NodeValue})
	_, err := r.publishRemotePodCIDRs(context.Background(), m, "worker")
	if err == nil || !strings.Contains(err.Error(), "no allocation observer") {
		t.Fatalf("publish = %v, want an error naming the missing observer", err)
	}
}

// An EC2 node whose template chose the site CNI carries no network label
// and is an ordinary site-CNI node, even where the VPC CNI is observed.
func TestAnEC2NodeOnTheSiteCNIIsNotAnError(t *testing.T) {
	r, m := nativeNetworkFixture(t, nil)
	r.nativeObservers = map[string]platform.Observer{awspod.NodeValue: platformaws.Observer{Observer: &awspod.Observer{}}}
	if _, err := r.publishRemotePodCIDRs(context.Background(), m, "worker"); err != nil {
		t.Fatalf("site-CNI EC2 node rejected: %v", err)
	}
	if err := r.ensureCNINodeAddress(context.Background(), "worker", []string{"10.100.0.128"}, ""); err != nil {
		t.Fatalf("site-CNI EC2 node rejected for its CNI address: %v", err)
	}
}
