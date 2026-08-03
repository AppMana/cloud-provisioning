package claim

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These tests use the REAL aws.Provider as the registered provisioner
// (catalog resolution, AWSMachine rendering from the provider-config
// Secret) against a fake API server -- the claim reconciler's whole
// job is expansion, and the expansion is only proven if the real
// provider's output shapes are what get created.

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding v1alpha1: %v", err)
	}
	scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(machineListGVK, &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(clusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(clusterListGVK, &unstructured.UnstructuredList{})
	awsMachineGVK := joinaws.Provider{}.GVK()
	scheme.AddKnownTypeWithName(awsMachineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(awsMachineGVK.GroupVersion().WithKind("AWSMachineList"), &unstructured.UnstructuredList{})
	return scheme
}

func fakeCluster(name string) *unstructured.Unstructured {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetName(name)
	cluster.SetNamespace("default")
	_ = unstructured.SetNestedField(cluster.Object, "AWSCluster", "spec", "infrastructureRef", "kind")
	_ = unstructured.SetNestedField(cluster.Object, name, "spec", "infrastructureRef", "name")
	return cluster
}

func awsConfigSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-provider-config", Namespace: "wg-dialer"},
		Data: map[string][]byte{
			"ami-arm64": []byte("ami-0abc"),
			"subnet-id": []byte("subnet-9"),
		},
	}
}

func fakeClaim(name string) *v1alpha1.ProvisionedNodeClaim {
	return &v1alpha1.ProvisionedNodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.ProvisionedNodeClaimSpec{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
			Arch: "arm64",
		},
	}
}

func newClaimReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&v1alpha1.ProvisionedNodeClaim{}).
		WithObjects(objs...).
		Build()
	return &Reconciler{
		Client:                    c,
		Reader:                    c,
		Provisioners:              []join.MachineProvisioner{joinaws.Provider{ConfigNamespace: "wg-dialer", ConfigName: "aws-provider-config"}},
		RoleLabel:                 "cloud-provisioning.appmana.com/role",
		RoleValue:                 "cloud-worker",
		BootstrapSecretNameFormat: "%s-bootstrap",
		TunnelInterface:           "cldt0a1b2c3d",
	}
}

func reconcileClaim(t *testing.T, r *Reconciler, claim *v1alpha1.ProvisionedNodeClaim) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(claim)})
	return err
}

func TestReconcile_ExpandsClaimIntoMachinePair(t *testing.T) {
	claim := fakeClaim("public-worker")
	r := newClaimReconciler(t, claim, fakeCluster("appmana"), awsConfigSecret())

	if err := reconcileClaim(t, r, claim); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The provider machine: rendered by the REAL aws.Provider from the
	// provider-config Secret, named after the claim, owned by it.
	awsMachine := &unstructured.Unstructured{}
	awsMachine.SetGroupVersionKind(joinaws.Provider{}.GVK())
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "public-worker"}, awsMachine); err != nil {
		t.Fatalf("expected an AWSMachine named after the claim: %v", err)
	}
	instanceType, _, _ := unstructured.NestedString(awsMachine.Object, "spec", "instanceType")
	if instanceType != "t4g.medium" {
		t.Errorf("AWSMachine instanceType = %q, want t4g.medium (smallest arm64 fit for 2cpu/4Gi)", instanceType)
	}
	ami, _, _ := unstructured.NestedString(awsMachine.Object, "spec", "ami", "id")
	if ami != "ami-0abc" {
		t.Errorf("AWSMachine ami = %q, want the provider-config's ami-arm64", ami)
	}
	if len(awsMachine.GetOwnerReferences()) != 1 || awsMachine.GetOwnerReferences()[0].Kind != "ProvisionedNodeClaim" {
		t.Errorf("AWSMachine ownerReferences = %+v, want exactly the claim (cascade delete)", awsMachine.GetOwnerReferences())
	}
	if awsMachine.GetLabels()["cluster.x-k8s.io/cluster-name"] != "appmana" {
		t.Errorf("AWSMachine cluster-name label = %q, want appmana", awsMachine.GetLabels()["cluster.x-k8s.io/cluster-name"])
	}

	// The CAPI Machine: dataSecretName preset (CAPA blocks RunInstances
	// until that Secret exists; the join reconciler creates it under
	// exactly this name), role label (the join controller's watch
	// selector), ownerRef to the claim.
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "public-worker"}, machine); err != nil {
		t.Fatalf("expected a Machine named after the claim: %v", err)
	}
	dataSecretName, _, _ := unstructured.NestedString(machine.Object, "spec", "bootstrap", "dataSecretName")
	if dataSecretName != "public-worker-bootstrap" {
		t.Errorf("Machine bootstrap.dataSecretName = %q, want public-worker-bootstrap", dataSecretName)
	}
	infraKind, _, _ := unstructured.NestedString(machine.Object, "spec", "infrastructureRef", "kind")
	infraName, _, _ := unstructured.NestedString(machine.Object, "spec", "infrastructureRef", "name")
	if infraKind != "AWSMachine" || infraName != "public-worker" {
		t.Errorf("Machine infrastructureRef = %s/%s, want AWSMachine/public-worker", infraKind, infraName)
	}
	clusterName, _, _ := unstructured.NestedString(machine.Object, "spec", "clusterName")
	if clusterName != "appmana" {
		t.Errorf("Machine clusterName = %q, want appmana", clusterName)
	}
	if machine.GetLabels()["cloud-provisioning.appmana.com/role"] != "cloud-worker" {
		t.Errorf("Machine role label = %q, want cloud-worker (the join/mesh controllers' watch selector)", machine.GetLabels()["cloud-provisioning.appmana.com/role"])
	}
	if len(machine.GetOwnerReferences()) != 1 || machine.GetOwnerReferences()[0].Kind != "ProvisionedNodeClaim" {
		t.Errorf("Machine ownerReferences = %+v, want exactly the claim", machine.GetOwnerReferences())
	}

	updated := &v1alpha1.ProvisionedNodeClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), updated); err != nil {
		t.Fatalf("getting updated claim: %v", err)
	}
	if updated.Status.InstanceType != "t4g.medium" || updated.Status.Provider != "AWSMachine" {
		t.Errorf("claim status = %+v, want InstanceType=t4g.medium Provider=AWSMachine", updated.Status)
	}
	if updated.Status.Phase != "Resolving" {
		t.Errorf("claim phase = %q, want Resolving before the Machine has any status", updated.Status.Phase)
	}
	if updated.Status.TunnelInterface != "cldt0a1b2c3d" {
		t.Errorf("claim tunnelInterface = %q, want the mesh interface name", updated.Status.TunnelInterface)
	}
}

func TestReconcile_SecondPassIsIdempotent(t *testing.T) {
	claim := fakeClaim("public-worker")
	r := newClaimReconciler(t, claim, fakeCluster("appmana"), awsConfigSecret())

	if err := reconcileClaim(t, r, claim); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if err := reconcileClaim(t, r, claim); err != nil {
		t.Fatalf("second Reconcile must be a no-op, got: %v", err)
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(machineListGVK)
	if err := r.List(context.Background(), list, client.InNamespace("default")); err != nil {
		t.Fatalf("listing Machines: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected exactly 1 Machine after two reconciles, got %d", len(list.Items))
	}
}

func TestReconcile_UnsatisfiableRequests_RecordsFailedStatus(t *testing.T) {
	claim := fakeClaim("public-worker")
	claim.Spec.Requests[corev1.ResourceMemory] = resource.MustParse("1Ti")
	r := newClaimReconciler(t, claim, fakeCluster("appmana"), awsConfigSecret())

	if err := reconcileClaim(t, r, claim); err == nil {
		t.Fatal("expected an error for an unsatisfiable request, got nil")
	}
	updated := &v1alpha1.ProvisionedNodeClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), updated); err != nil {
		t.Fatalf("getting updated claim: %v", err)
	}
	if updated.Status.Phase != "Failed" {
		t.Errorf("claim phase = %q, want Failed", updated.Status.Phase)
	}
	if !strings.Contains(updated.Status.Message, "instance type") {
		t.Errorf("claim message %q should say no instance type satisfies the request", updated.Status.Message)
	}
}

func TestReconcile_NoClusterInNamespace_Fails(t *testing.T) {
	claim := fakeClaim("public-worker")
	r := newClaimReconciler(t, claim, awsConfigSecret())

	if err := reconcileClaim(t, r, claim); err == nil {
		t.Fatal("expected an error when no CAPI Cluster exists, got nil")
	}
	updated := &v1alpha1.ProvisionedNodeClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), updated); err != nil {
		t.Fatalf("getting updated claim: %v", err)
	}
	if updated.Status.Phase != "Failed" {
		t.Errorf("claim phase = %q, want Failed", updated.Status.Phase)
	}
}

func TestReconcile_TwoClustersWithoutClusterName_IsAnErrorNotAGuess(t *testing.T) {
	claim := fakeClaim("public-worker")
	r := newClaimReconciler(t, claim, fakeCluster("appmana"), fakeCluster("other"), awsConfigSecret())

	err := reconcileClaim(t, r, claim)
	if err == nil {
		t.Fatal("expected an error with two Clusters and no spec.clusterName, got nil")
	}
	if !strings.Contains(err.Error(), "clusterName") {
		t.Errorf("error %q must tell the user to set spec.clusterName", err.Error())
	}
}

func TestReconcile_ExplicitClusterNameSelectsAmongMany(t *testing.T) {
	claim := fakeClaim("public-worker")
	claim.Spec.ClusterName = "other"
	r := newClaimReconciler(t, claim, fakeCluster("appmana"), fakeCluster("other"), awsConfigSecret())

	if err := reconcileClaim(t, r, claim); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "public-worker"}, machine); err != nil {
		t.Fatalf("expected a Machine: %v", err)
	}
	clusterName, _, _ := unstructured.NestedString(machine.Object, "spec", "clusterName")
	if clusterName != "other" {
		t.Errorf("Machine clusterName = %q, want the explicitly selected \"other\"", clusterName)
	}
}

func TestReconcile_NoProvisionerForClusterKind_Fails(t *testing.T) {
	claim := fakeClaim("public-worker")
	cluster := fakeCluster("appmana")
	_ = unstructured.SetNestedField(cluster.Object, "GCPCluster", "spec", "infrastructureRef", "kind")
	r := newClaimReconciler(t, claim, cluster, awsConfigSecret())

	err := reconcileClaim(t, r, claim)
	if err == nil {
		t.Fatal("expected an error when no registered provider fulfills the cluster's infrastructure kind, got nil")
	}
	if !strings.Contains(err.Error(), "GCPCluster") {
		t.Errorf("error %q must name the unfulfillable kind", err.Error())
	}
}
