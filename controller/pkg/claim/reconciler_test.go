package claim

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
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

// fakeNode provides the version-introspection source every expansion
// needs (Machine.spec.version comes from the live cluster). The +k0s
// build suffix must be stripped: Machine versions are plain semver.
func fakeNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "onprem-0"},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.36.2+k0s"}},
	}
}

var infraGroup = "infrastructure.cluster.x-k8s.io"

// fakeTemplate is the machine template a claim points at: ordinary
// Cluster API, holding the machine's spec where the contract says.
func fakeTemplate(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "AWSMachineTemplate",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"instanceType": "t3.micro",
			"ami":          map[string]any{"id": "ami-000000000000000ab"},
			"additionalSecurityGroups": []any{
				map[string]any{"id": "sg-000000000000000cd"},
			},
		}}},
	}}
}

func fakeClaim(name string) *v1alpha1.ProvisionedNodeClaim {
	return &v1alpha1.ProvisionedNodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.ProvisionedNodeClaimSpec{
			InfrastructureRef: corev1.TypedLocalObjectReference{
				APIGroup: &infraGroup, Kind: "AWSMachineTemplate", Name: name,
			},
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
	r := newClaimReconciler(t, claim, fakeTemplate(claim.Name), fakeCluster("appmana"), awsConfigSecret(), fakeNode())

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
	if instanceType != "t3.micro" {
		t.Errorf("AWSMachine instanceType = %q, want the template's t3.micro", instanceType)
	}
	ami, _, _ := unstructured.NestedString(awsMachine.Object, "spec", "ami", "id")
	if ami != "ami-000000000000000ab" {
		t.Errorf("AWSMachine ami = %q, want the template's ami", ami)
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
	version, _, _ := unstructured.NestedString(machine.Object, "spec", "version")
	if version != "v1.36.2" {
		t.Errorf("Machine version = %q, want the live node's version with the +k0s build suffix stripped", version)
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
	if updated.Status.InstanceType != "t3.micro" || updated.Status.Provider != "AWSMachine" {
		t.Errorf("claim status = %+v, want InstanceType=t3.micro Provider=AWSMachine", updated.Status)
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
	r := newClaimReconciler(t, claim, fakeTemplate(claim.Name), fakeCluster("appmana"), awsConfigSecret(), fakeNode())

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

func TestReconcile_NoClusterInNamespace_Fails(t *testing.T) {
	claim := fakeClaim("public-worker")
	r := newClaimReconciler(t, claim, awsConfigSecret(), fakeNode())

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
	r := newClaimReconciler(t, claim, fakeTemplate(claim.Name), fakeCluster("appmana"), fakeCluster("other"), awsConfigSecret(), fakeNode())

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
	r := newClaimReconciler(t, claim, fakeTemplate(claim.Name), fakeCluster("appmana"), fakeCluster("other"), awsConfigSecret(), fakeNode())

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
	r := newClaimReconciler(t, claim, cluster, awsConfigSecret(), fakeNode())

	err := reconcileClaim(t, r, claim)
	if err == nil {
		t.Fatal("expected an error when no registered provider fulfills the cluster's infrastructure kind, got nil")
	}
	if !strings.Contains(err.Error(), "GCPCluster") {
		t.Errorf("error %q must name the unfulfillable kind", err.Error())
	}
}

func TestReconcile_MachineDeletionTimeoutsAreBounded(t *testing.T) {
	// CAPI's defaults wait forever on drain/detach/node-deletion. The
	// node a claim creates is reachable ONLY through the tunnel being
	// torn down, so it is exactly the node that can become undrainable
	// mid-deletion -- unbounded, `kubectl delete provisionednodeclaim`
	// would hang forever with a billed instance still running.
	claim := fakeClaim("public-worker")
	r := newClaimReconciler(t, claim, fakeTemplate(claim.Name), fakeCluster("appmana"), awsConfigSecret(), fakeNode())

	if err := reconcileClaim(t, r, claim); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "public-worker"}, machine); err != nil {
		t.Fatalf("getting Machine: %v", err)
	}
	for _, field := range []string{"nodeDrainTimeoutSeconds", "nodeVolumeDetachTimeoutSeconds", "nodeDeletionTimeoutSeconds"} {
		v, found, err := unstructured.NestedInt64(machine.Object, "spec", "deletion", field)
		if err != nil || !found {
			t.Errorf("spec.deletion.%s not set -- deletion would block indefinitely", field)
			continue
		}
		if v <= 0 {
			t.Errorf("spec.deletion.%s = %d, want a positive bound", field, v)
		}
	}
}

func TestReconcileDelete_RemovesComputeAndOnlyThenReleasesTheClaim(t *testing.T) {
	// OwnerRef GC is NOT the teardown mechanism: CAPI's own Machine
	// controller reconciles ownerReferences and replaces the claim's
	// with the Cluster, so a deleted claim left the Machine Running
	// with a billed instance (confirmed live). The claim holds a
	// finalizer and deletes the compute itself.
	claim := fakeClaim("public-worker")
	r := newClaimReconciler(t, claim, fakeTemplate(claim.Name), fakeCluster("appmana"), awsConfigSecret(), fakeNode())
	if err := reconcileClaim(t, r, claim); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	created := &v1alpha1.ProvisionedNodeClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), created); err != nil {
		t.Fatalf("getting claim: %v", err)
	}
	if !containsString(created.Finalizers, claimFinalizer) {
		t.Fatalf("claim finalizers = %v, want %s -- without it deletion orphans the compute", created.Finalizers, claimFinalizer)
	}

	// Reproduce what CAPI does: strip the claim's ownerRef from the
	// Machine, so this test can only pass via explicit deletion.
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "public-worker"}, machine); err != nil {
		t.Fatalf("getting Machine: %v", err)
	}
	machine.SetOwnerReferences(nil)
	if err := r.Update(context.Background(), machine); err != nil {
		t.Fatalf("stripping ownerRefs: %v", err)
	}

	if err := r.Delete(context.Background(), created); err != nil {
		t.Fatalf("deleting claim: %v", err)
	}
	// The fake client honors finalizers, so the claim still exists.
	if err := reconcileClaim(t, r, created); err != nil {
		t.Fatalf("Reconcile(deleting): %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "public-worker"}, machine); !apierrors.IsNotFound(err) {
		t.Errorf("Machine still present after teardown reconcile: %v", err)
	}
	awsMachine := &unstructured.Unstructured{}
	awsMachine.SetGroupVersionKind(joinaws.Provider{}.GVK())
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "public-worker"}, awsMachine); !apierrors.IsNotFound(err) {
		t.Errorf("provider machine still present after teardown reconcile: %v", err)
	}

	// Compute gone -> the finalizer is released and the claim goes away.
	if err := reconcileClaim(t, r, created); err != nil {
		t.Fatalf("Reconcile(final): %v", err)
	}
	remaining := &v1alpha1.ProvisionedNodeClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), remaining); !apierrors.IsNotFound(err) {
		t.Errorf("claim survived teardown (finalizer never released): %v, finalizers=%v", err, remaining.Finalizers)
	}
}

// A template describes the machine completely, including the security
// groups and subnet that decide whether the tunnel can be established
// at all. Nothing about it may be second-guessed from the claim.
// The template describes the machine completely, including the
// security groups and subnet that decide whether the tunnel can be
// established at all. Nothing about it may be second-guessed.
func TestReconcile_TemplateDrivesTheMachine(t *testing.T) {
	claim := fakeClaim("public-worker")
	r := newClaimReconciler(t, claim, fakeTemplate("public-worker"), fakeCluster("appmana"), fakeNode())
	if err := reconcileClaim(t, r, claim); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSMachine",
	})
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "public-worker"}, machine); err != nil {
		t.Fatalf("getting AWSMachine: %v", err)
	}
	if got, _, _ := unstructured.NestedString(machine.Object, "spec", "instanceType"); got != "t3.micro" {
		t.Errorf("instanceType = %q, want the template's t3.micro", got)
	}
	if ami, _, _ := unstructured.NestedString(machine.Object, "spec", "ami", "id"); ami != "ami-000000000000000ab" {
		t.Errorf("ami = %q, want the template's", ami)
	}
	// The security groups are the reason the template exists: carried
	// here, reachability never depends on an out-of-band change.
	sgs, found, _ := unstructured.NestedSlice(machine.Object, "spec", "additionalSecurityGroups")
	if !found || len(sgs) != 1 {
		t.Fatalf("additionalSecurityGroups = %v, want the template's one entry", sgs)
	}
}

// The machine kind is the template kind without the suffix, which is
// Cluster API's contract, so any provider's template works with no code
// here knowing the type.
func TestReconcile_MachineKindComesFromTheTemplateKind(t *testing.T) {
	claim := fakeClaim("public-worker")
	claim.Spec.InfrastructureRef.Kind = "DockerMachineTemplate"
	template := fakeTemplate("public-worker")
	template.SetKind("DockerMachineTemplate")
	unstructured.SetNestedMap(template.Object, map[string]any{"customImage": "kindest/node:v1.34.0"}, "spec", "template", "spec")

	r := newClaimReconciler(t, claim, template, fakeCluster("appmana"), fakeNode())
	if err := reconcileClaim(t, r, claim); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "DockerMachine",
	})
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "public-worker"}, machine); err != nil {
		t.Fatalf("expected a DockerMachine from a DockerMachineTemplate: %v", err)
	}
	if img, _, _ := unstructured.NestedString(machine.Object, "spec", "customImage"); img != "kindest/node:v1.34.0" {
		t.Errorf("customImage = %q, want the template's", img)
	}
}

// Both set is ambiguous: the catalogue would silently override half of
// what the template says.

// Deleting a claim used to leave its Node registered and NotReady
// forever: nothing collects it, because a cluster-scoped Node cannot be
// owned by a namespaced claim and Cluster API only removes nodes for
// clusters it manages.
func TestReconcileDelete_RemovesTheNodeItProduced(t *testing.T) {
	claim := fakeClaim("public-worker")
	claim.Finalizers = []string{claimFinalizer}
	now := metav1.Now()
	claim.DeletionTimestamp = &now

	provisioned := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "ip-10-0-0-1",
		Annotations: map[string]string{ClaimAnnotation: "default/public-worker"},
	}}
	other := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}

	r := newClaimReconciler(t, claim, fakeTemplate(claim.Name), fakeCluster("appmana"), provisioned, other)
	if err := reconcileClaim(t, r, claim); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if err := r.Get(context.Background(), client.ObjectKey{Name: "ip-10-0-0-1"}, &corev1.Node{}); !apierrors.IsNotFound(err) {
		t.Errorf("the provisioned Node survived claim teardown (err = %v)", err)
	}
	// Every other node in the cluster must be untouched.
	if err := r.Get(context.Background(), client.ObjectKey{Name: "worker-1"}, &corev1.Node{}); err != nil {
		t.Errorf("an unrelated node was removed: %v", err)
	}
}
