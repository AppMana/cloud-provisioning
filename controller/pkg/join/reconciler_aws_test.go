// Package join_test: Reconcile-level tests that need the REAL
// aws.Provider. These live in an external test package because
// pkg/join/aws imports pkg/join (NodeRequest/MachineProvisioner) --
// importing it back from an in-package test file would be an import
// cycle.
package join_test

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var machineGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}

type countingJoinProvider struct{ calls int }

func (s *countingJoinProvider) JoinValues(ctx context.Context) (map[string]any, error) {
	s.calls++
	return map[string]any{}, nil
}

func TestReconcile_InfraProviderValidateErrorBlocksBootstrapSecretCreation(t *testing.T) {
	// Full Reconcile-level proof of the aws.Validator wiring, using the
	// real aws.Provider (not a stub): reproduces the exact live bug
	// caught against a live cluster (AWSClusterStaticIdentity's
	// secretRef Secret in the wrong namespace) and confirms Reconcile
	// surfaces it as an immediate error instead of proceeding to create
	// a bootstrap secret that CAPA could never actually use.
	clusterGVK := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}
	awsClusterGVK := schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSCluster"}
	awsClusterStaticIdentityGVK := schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSClusterStaticIdentity"}
	machineListGVK := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineList"}
	awsProvider := joinaws.Provider{}

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	machine.SetName("cloud-worker-0")
	machine.SetNamespace("default")
	machine.SetLabels(map[string]string{"cluster.x-k8s.io/cluster-name": "example-cluster"})
	_ = unstructured.SetNestedField(machine.Object, "cloud-worker-0", "spec", "infrastructureRef", "name")
	_ = unstructured.SetNestedField(machine.Object, "AWSMachine", "spec", "infrastructureRef", "kind")

	awsMachine := &unstructured.Unstructured{}
	awsMachine.SetGroupVersionKind(awsProvider.GVK())
	awsMachine.SetName("cloud-worker-0")
	awsMachine.SetNamespace("default")
	awsMachine.SetLabels(map[string]string{"cluster.x-k8s.io/cluster-name": "example-cluster"})

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetName("example-cluster")
	cluster.SetNamespace("default")
	_ = unstructured.SetNestedField(cluster.Object, "AWSCluster", "spec", "infrastructureRef", "kind")
	_ = unstructured.SetNestedField(cluster.Object, "example-cluster", "spec", "infrastructureRef", "name")

	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(awsClusterGVK)
	awsCluster.SetName("example-cluster")
	awsCluster.SetNamespace("default")
	_ = unstructured.SetNestedField(awsCluster.Object, "AWSClusterStaticIdentity", "spec", "identityRef", "kind")
	_ = unstructured.SetNestedField(awsCluster.Object, "cloud-worker", "spec", "identityRef", "name")

	identity := &unstructured.Unstructured{}
	identity.SetGroupVersionKind(awsClusterStaticIdentityGVK)
	identity.SetName("cloud-worker")
	_ = unstructured.SetNestedField(identity.Object, "cloud-worker-credentials", "spec", "secretRef")

	// The bug, reproduced: credentials Secret only in "default", never
	// in "capa-system" (CAPA's actual manager namespace).
	wrongNamespaceSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cloud-worker-credentials", Namespace: "default"},
	}

	dialerSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wg-dialer-peer", Namespace: "wg-dialer"},
		Data: map[string][]byte{
			"node-public-key-worker-1":     []byte("c3BhcmsyYWIzLXB1YmxpYy1rZXktdGVzdC1vbmx5PT0="),
			"node-tunnel-address-worker-1": []byte("10.100.0.1/24"),
			"node-cluster-vips-worker-1":   []byte("10.101.0.2"),
		},
	}
	joinProvider := &countingJoinProvider{}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(machineListGVK, &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(awsProvider.GVK(), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(clusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(awsClusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(awsClusterStaticIdentityGVK, &unstructured.Unstructured{})
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine, awsMachine, dialerSecret, cluster, awsCluster, identity, wrongNamespaceSecret).
		Build()

	r := &join.Reconciler{
		Client:                    c,
		Reader:                    c,
		Join:                      joinProvider,
		InfraProviders:            []join.InfraProvider{awsProvider},
		NodeVIP4Prefix:            "10.101.0.",
		NodeVIP6Prefix:            "fd8f:cf26:522a::",
		NodeVIPStart:              4,
		WireGuardAddress:          "10.100.0.128/24",
		DialerPeerSecretNamespace: "wg-dialer",
		DialerPeerSecretName:      "wg-dialer-peer",
		DialerBinaryURLARM64:      "https://example.com/wg-dialer-linux-arm64",
		DialerBinarySHA256ARM64:   "deadbeef",
		BootstrapSecretNameFormat: "%s-bootstrap",
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	if err == nil {
		t.Fatal("expected Reconcile to surface the misplaced-identity-secret error, got nil")
	}
	if !strings.Contains(err.Error(), "capa-system") {
		t.Errorf("error %q doesn't mention the correct namespace -- not actionable", err.Error())
	}
	if joinProvider.calls != 0 {
		t.Errorf("JoinValues must not be called when infra validation fails, got %d calls", joinProvider.calls)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cloud-worker-0-bootstrap"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected no bootstrap secret to be created when infra validation fails, Get returned: %v", err)
	}
}
