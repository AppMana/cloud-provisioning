package claim

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	attachmentaws "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/bootstrap"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni/site"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	joingcp "github.com/appmana/cloud-provisioning/controller/pkg/join/gcp"
	joingeneric "github.com/appmana/cloud-provisioning/controller/pkg/join/generic"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These tests use the real aws.Provider as the registered provisioner
// (catalog resolution, AWSMachine rendering from the provider-config
// Secret) against a fake API server. The claim reconciler's job is
// expansion, and the expansion is only proven if the real provider's
// output shapes are what get created.

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
	v1beta2GV := schema.GroupVersion{Group: infraGroup, Version: "v1beta2"}
	for _, kind := range []string{
		"AWSMachine", "AWSMachineTemplate", "AWSCluster", "AWSClusterStaticIdentity", "AWSClusterRoleIdentity",
		"DockerMachine", "DockerMachineTemplate",
	} {
		scheme.AddKnownTypeWithName(v1beta2GV.WithKind(kind), &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(v1beta2GV.WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	v1beta1GV := schema.GroupVersion{Group: infraGroup, Version: "v1beta1"}
	for _, kind := range []string{
		"GCPMachine", "GCPMachineTemplate", "GCPCluster",
		"AzureMachine", "AzureMachineTemplate", "AzureCluster",
	} {
		scheme.AddKnownTypeWithName(v1beta1GV.WithKind(kind), &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(v1beta1GV.WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	return scheme
}

// servedBy maps every kind the scheme knows, as the API server serves
// the kinds whose CRDs are installed.
func servedBy(scheme *runtime.Scheme) apimeta.RESTMapper {
	mapper := apimeta.NewDefaultRESTMapper(scheme.PrioritizedVersionsAllGroups())
	for gvk := range scheme.AllKnownTypes() {
		scope := apimeta.RESTScopeNamespace
		if gvk.Kind == "Node" || gvk.Kind == "Namespace" {
			scope = apimeta.RESTScopeRoot
		}
		mapper.Add(gvk, scope)
	}
	return mapper
}

func fakeCluster(name string) *unstructured.Unstructured {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetName(name)
	cluster.SetNamespace("default")
	_ = unstructured.SetNestedField(cluster.Object, "AWSCluster", "spec", "infrastructureRef", "kind")
	_ = unstructured.SetNestedField(cluster.Object, infraGroup, "spec", "infrastructureRef", "apiGroup")
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
	scheme := testScheme(t)
	mapper := servedBy(scheme)
	// Every Cluster's infrastructure Cluster, unless the test gives one.
	for _, obj := range objs {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "Cluster" {
			name, _, _ := unstructured.NestedString(u.Object, "spec", "infrastructureRef", "name")
			kind, _, _ := unstructured.NestedString(u.Object, "spec", "infrastructureRef", "kind")
			if kind == "" {
				kind = "AWSCluster"
			}
			ver := "v1beta2"
			if m, err := mapper.RESTMapping(schema.GroupKind{Group: infraGroup, Kind: kind}); err == nil && m.GroupVersionKind.Version != "" {
				ver = m.GroupVersionKind.Version
			}
			infra := &unstructured.Unstructured{}
			infra.SetGroupVersionKind(schema.GroupVersionKind{Group: infraGroup, Version: ver, Kind: kind})
			infra.SetNamespace(u.GetNamespace())
			infra.SetName(name)
			_ = unstructured.SetNestedField(infra.Object, "us-west-2", "spec", "region")
			present := false
			for _, other := range objs {
				present = present || other.GetObjectKind().GroupVersionKind().Kind == kind && other.GetName() == name
			}
			if !present {
				objs = append(objs, infra)
			}
		}
	}
	statusSubresources := []client.Object{&v1alpha1.ProvisionedNodeClaim{}}
	uCluster := &unstructured.Unstructured{}
	uCluster.SetGroupVersionKind(clusterGVK)
	statusSubresources = append(statusSubresources, uCluster)
	for _, gvk := range []schema.GroupVersionKind{
		{Group: infraGroup, Version: "v1beta2", Kind: "AWSCluster"},
		{Group: infraGroup, Version: "v1beta1", Kind: "GCPCluster"},
		{Group: infraGroup, Version: "v1beta1", Kind: "AzureCluster"},
	} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		statusSubresources = append(statusSubresources, u)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(mapper).
		WithStatusSubresource(statusSubresources...).
		WithObjects(objs...).
		Build()
	return &Reconciler{
		Client:                    c,
		Reader:                    c,
		Provisioners:              []join.MachineProvisioner{joinaws.Provider{ConfigNamespace: "wg-dialer", ConfigName: "aws-provider-config", EC2: admittingGroups}},
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
	claim.UID = "creation-claim-uid"
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
	if machine.GetAnnotations()[ClaimAnnotation] != claim.Namespace+"/"+claim.Name || machine.GetAnnotations()[ClaimUIDAnnotation] != string(claim.UID) {
		t.Fatal("Machine creation lost claim incarnation")
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
	// CAPI's defaults are unbounded on drain/detach/node-deletion. The
	// node a claim creates is reachable only through the tunnel being
	// torn down, so it can become undrainable mid-deletion; unbounded,
	// `kubectl delete provisionednodeclaim` would hang with its
	// instance still running.
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
			t.Errorf("spec.deletion.%s not set: deletion would block indefinitely", field)
			continue
		}
		if v <= 0 {
			t.Errorf("spec.deletion.%s = %d, want a positive bound", field, v)
		}
	}
}

func TestReconcileDelete_RemovesComputeAndOnlyThenReleasesTheClaim(t *testing.T) {
	// OwnerRef GC is not the teardown mechanism: CAPI's own Machine
	// controller reconciles ownerReferences and replaces the claim's
	// with the Cluster, so a deleted claim would leave the Machine
	// Running with its instance up. The claim holds a finalizer and
	// deletes the compute itself.
	claim := fakeClaim("public-worker")
	r := newClaimReconciler(t, claim, fakeTemplate(claim.Name), fakeCluster("appmana"), awsConfigSecret(), fakeNode())
	if err := reconcileClaim(t, r, claim); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	created := &v1alpha1.ProvisionedNodeClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), created); err != nil {
		t.Fatalf("getting claim: %v", err)
	}
	if !containsString(created.Finalizers, Finalizer) {
		t.Fatalf("claim finalizers = %v, want %s: without it deletion orphans the compute", created.Finalizers, Finalizer)
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
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "public-worker"}, awsMachine); err != nil {
		t.Fatalf("provider must survive the Machine deletion request: %v", err)
	}
	// This fake has no CAPI controller; after observing the Machine absent,
	// the claim controller can collect the orphaned infrastructure object.
	if err := reconcileClaim(t, r, created); err != nil {
		t.Fatalf("Reconcile(orphan cleanup): %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "public-worker"}, awsMachine); !apierrors.IsNotFound(err) {
		t.Errorf("provider machine still present after teardown reconcile: %v", err)
	}

	// Once the compute is gone the finalizer is released and the claim goes away.
	if err := reconcileClaim(t, r, created); err != nil {
		t.Fatalf("Reconcile(final): %v", err)
	}
	remaining := &v1alpha1.ProvisionedNodeClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), remaining); !apierrors.IsNotFound(err) {
		t.Errorf("claim survived teardown (finalizer never released): %v, finalizers=%v", err, remaining.Finalizers)
	}
}

// Fake API state-machine coverage only: conditions and provider states are
// injected snapshots, not proof that CAPA observes EC2 faithfully or that
// provider termination is timely.
func TestReconcileDelete_PreservesInfrastructureWhileCAPIDeletionIsPending(t *testing.T) {
	for _, providerState := range []string{"", "running", "shutting-down", "terminated"} {
		for _, condition := range []string{"True", "False", "Unknown"} {
			t.Run("provider-"+providerState+"/reported-ready-"+condition, func(t *testing.T) {
				ctx := context.Background()
				claim := fakeClaim("public-worker")
				r := newClaimReconciler(t, claim, fakeTemplate(claim.Name), fakeCluster("appmana"), fakeNode())
				if err := reconcileClaim(t, r, claim); err != nil {
					t.Fatal(err)
				}
				key := client.ObjectKeyFromObject(claim)
				provider := &unstructured.Unstructured{}
				provider.SetGroupVersionKind(joinaws.Provider{}.GVK())
				if err := r.Get(ctx, key, provider); err != nil {
					t.Fatal(err)
				}
				// Provider status is an asynchronous observation, not authority to
				// bypass either deletion lifecycle, even when it says terminated.
				if providerState != "" {
					if err := unstructured.SetNestedField(provider.Object, providerState, "status", "instanceState"); err != nil {
						t.Fatal(err)
					}
					if err := r.Update(ctx, provider); err != nil {
						t.Fatal(err)
					}
				}
				machine := &unstructured.Unstructured{}
				machine.SetGroupVersionKind(machineGVK)
				if err := r.Get(context.Background(), key, machine); err != nil {
					t.Fatal(err)
				}
				// Conditions can lag the provider or describe a failed reconcile. Their
				// values must never authorize bypassing the CAPI deletion lifecycle.
				if err := unstructured.SetNestedSlice(machine.Object, []any{map[string]any{"type": "Ready", "status": condition}}, "status", "conditions"); err != nil {
					t.Fatal(err)
				}
				machine.SetFinalizers([]string{"machine.cluster.x-k8s.io"})
				machine.SetAnnotations(map[string]string{"pre-drain.delete.hook.machine.cluster.x-k8s.io/test": "awaiting-network-withdrawal"})
				if err := r.Update(context.Background(), machine); err != nil {
					t.Fatal(err)
				}
				created := &v1alpha1.ProvisionedNodeClaim{}
				if err := r.Get(context.Background(), key, created); err != nil {
					t.Fatal(err)
				}
				if err := r.Delete(context.Background(), created); err != nil {
					t.Fatal(err)
				}
				for attempt := 0; attempt < 3; attempt++ {
					// Preserve API state but discard controller process state each time.
					restarted := *r
					result, err := restarted.Reconcile(ctx, ctrl.Request{NamespacedName: key})
					if err != nil || result.RequeueAfter != 10*time.Second {
						t.Fatalf("CAPI pending must retry without error backoff: %+v %v", result, err)
					}
					if err := r.Get(context.Background(), key, machine); err != nil || machine.GetDeletionTimestamp() == nil {
						t.Fatalf("CAPI Machine must remain pending deletion: %v", err)
					}
					infra := &unstructured.Unstructured{}
					infra.SetGroupVersionKind(joinaws.Provider{}.GVK())
					if err := r.Get(context.Background(), key, infra); err != nil || infra.GetDeletionTimestamp() != nil {
						t.Fatalf("claim bypassed CAPI's deletion gate: %v", err)
					}
					if err := r.Get(context.Background(), key, created); err != nil || !containsString(created.Finalizers, Finalizer) {
						t.Fatalf("claim must retain cleanup ownership: %v", err)
					}
				}
				// Model CAPI finishing Machine deletion while a provider object remains.
				// The claim may clean up that orphan, but must wait for its finalizer.
				infra := &unstructured.Unstructured{}
				infra.SetGroupVersionKind(joinaws.Provider{}.GVK())
				if err := r.Get(ctx, key, infra); err != nil {
					t.Fatal(err)
				}
				infra.SetFinalizers([]string{"test/provider-termination"})
				if err := r.Update(ctx, infra); err != nil {
					t.Fatal(err)
				}
				machine.SetFinalizers(nil)
				if err := r.Update(ctx, machine); err != nil {
					t.Fatal(err)
				}
				for attempt := 0; attempt < 3; attempt++ {
					restarted := *r
					result, err := restarted.Reconcile(ctx, ctrl.Request{NamespacedName: key})
					if err != nil || result.RequeueAfter != 10*time.Second {
						t.Fatalf("provider finalizer must remain pending: %+v %v", result, err)
					}
					if err := r.Get(ctx, key, infra); err != nil || infra.GetDeletionTimestamp() == nil {
						t.Fatalf("orphan cleanup not requested: %v", err)
					}
					if observed, _, err := unstructured.NestedString(infra.Object, "status", "instanceState"); err != nil || observed != providerState {
						t.Fatalf("provider observation changed or was not exercised: got %q, want %q: %v", observed, providerState, err)
					}
					if err := r.Get(ctx, key, created); err != nil || !containsString(created.Finalizers, Finalizer) {
						t.Fatalf("provider pending lost claim ownership: %v", err)
					}
				}
				infra.SetFinalizers(nil)
				if err := r.Update(ctx, infra); err != nil {
					t.Fatal(err)
				}
				if err := reconcileClaim(t, r, created); err != nil {
					t.Fatal(err)
				}
				if err := r.Get(ctx, key, created); !apierrors.IsNotFound(err) {
					t.Fatalf("compute gone but claim did not finish: %v", err)
				}
				// A delayed duplicate event after completion is harmless.
				if err := reconcileClaim(t, r, created); err != nil {
					t.Fatal(err)
				}

			})
		}
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
	template := fakeTemplate("public-worker")
	_ = unstructured.SetNestedStringMap(template.Object, map[string]string{"kubernetes.io/os": "windows", clusterNameLabel: "must-not-override-cluster"}, "spec", "template", "metadata", "labels")
	_ = unstructured.SetNestedStringMap(template.Object, map[string]string{"image.example/recipe": "windows-2025"}, "spec", "template", "metadata", "annotations")
	r := newClaimReconciler(t, claim, template, fakeCluster("appmana"), fakeNode())
	r.Provisioners = []join.MachineProvisioner{joinaws.Provider{ConfigNamespace: "wg-dialer", ConfigName: "aws-provider-config", EC2: admittingGroups, WindowsIPAM: true}}
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
	if machine.GetLabels()["kubernetes.io/os"] != "windows" || machine.GetLabels()[clusterNameLabel] != "appmana" || machine.GetAnnotations()["image.example/recipe"] != "windows-2025" {
		t.Errorf("template guest metadata or controller-owned cluster label lost: labels=%v annotations=%v", machine.GetLabels(), machine.GetAnnotations())
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

// Claim deletion must remove the workload Node explicitly. A namespaced
// claim cannot own a cluster-scoped Node, and an externally managed cluster
// cannot rely on CAPI to collect it.
func TestReconcileDelete_RemovesTheNodeItProduced(t *testing.T) {
	claim := fakeClaim("public-worker")
	claim.Finalizers = []string{Finalizer}
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

// A claim whose node class the platform cannot run fails before any
// machine exists; an admitted class is recorded for operators.
func TestReconcile_NodeClassIsCheckedBeforeTheMachineExists(t *testing.T) {
	unsupported := fakeClaim("windows-arm")
	template := fakeTemplate("windows-arm")
	_ = unstructured.SetNestedStringMap(template.Object, map[string]string{"kubernetes.io/os": "windows", "kubernetes.io/arch": "arm64"}, "spec", "template", "metadata", "labels")
	r := newClaimReconciler(t, unsupported, template, fakeCluster("appmana"), fakeNode())
	if err := reconcileClaim(t, r, unsupported); err == nil {
		t.Fatal("Windows on arm64 with the VPC CNI was admitted")
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(joinaws.Provider{}.GVK())
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "windows-arm"}, machine); !apierrors.IsNotFound(err) {
		t.Fatalf("an AWSMachine exists for a rejected class: %v", err)
	}
	got := &v1alpha1.ProvisionedNodeClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(unsupported), got); err != nil || got.Status.Phase != "Failed" {
		t.Fatalf("claim status = %+v, %v; want Failed", got.Status, err)
	}

	// Windows on the VPC CNI needs Windows IPAM, without which its pods
	// would get no addresses.
	noIPAM := fakeClaim("windows-no-ipam")
	template = fakeTemplate("windows-no-ipam")
	_ = unstructured.SetNestedStringMap(template.Object, map[string]string{"kubernetes.io/os": "windows", "kubernetes.io/arch": "amd64"}, "spec", "template", "metadata", "labels")
	r = newClaimReconciler(t, noIPAM, template, fakeCluster("appmana"), fakeNode())
	if err := reconcileClaim(t, r, noIPAM); err == nil || !strings.Contains(err.Error(), "Windows IPAM") {
		t.Fatalf("a Windows VPC CNI claim without Windows IPAM: %v", err)
	}

	admitted := fakeClaim("linux-arm")
	template = fakeTemplate("linux-arm")
	_ = unstructured.SetNestedStringMap(template.Object, map[string]string{"kubernetes.io/arch": "arm64"}, "spec", "template", "metadata", "labels")
	r = newClaimReconciler(t, admitted, template, fakeCluster("appmana"), fakeNode())
	if err := reconcileClaim(t, r, admitted); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(admitted), got); err != nil {
		t.Fatal(err)
	}
	want := v1alpha1.NodeClass{OS: "linux", Arch: "arm64", CNI: "aws-vpc"}
	if got.Status.NodeClass != want {
		t.Fatalf("status.nodeClass = %+v, want %+v", got.Status.NodeClass, want)
	}
}

// A tunnel-class machine waits for the site network too: on a Calico site
// the product's Calico agent and site-only BGP peering come first.
func TestReconcile_TunnelMachinesWaitForTheSiteNetwork(t *testing.T) {
	claim := fakeClaim("tunnel")
	template := fakeTemplate("tunnel")
	_ = unstructured.SetNestedStringMap(template.Object, map[string]string{platform.CNILabel: platform.SiteCNI}, "spec", "template", "metadata", "labels")
	r := newClaimReconciler(t, claim, template, fakeCluster("appmana"), fakeNode())
	var asked platform.Class
	r.SiteNetwork = func(_ context.Context, class platform.Class) error {
		asked = class
		return &site.PendingError{DaemonSets: []string{"kube-system/calico-node-tunnel (not yet created)"}}
	}
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(claim)})
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("Reconcile = %+v, %v; want a timed requeue", result, err)
	}
	if asked.Native() {
		t.Fatalf("the gate was asked about class %+v", asked)
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(joinaws.Provider{}.GVK())
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "tunnel"}, machine); !apierrors.IsNotFound(err) {
		t.Fatalf("an AWSMachine exists before the site network admits it: %v", err)
	}
}

// A native-network machine is not created while the site pod network could
// still run on it; the claim says what it waits for, then proceeds.
func TestReconcile_NativeNetworkMachinesWaitForTheSiteNetworkExclusion(t *testing.T) {
	claim := fakeClaim("native")
	r := newClaimReconciler(t, claim, fakeTemplate("native"), fakeCluster("appmana"), fakeNode())
	waiting := &site.PendingError{DaemonSets: []string{"kube-system/calico-node"}}
	r.SiteNetwork = func(context.Context, platform.Class) error { return waiting }
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(claim)})
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("Reconcile = %+v, %v; want a timed requeue", result, err)
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(joinaws.Provider{}.GVK())
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "native"}, machine); !apierrors.IsNotFound(err) {
		t.Fatalf("an AWSMachine exists before the site network excludes it: %v", err)
	}
	got := &v1alpha1.ProvisionedNodeClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != "Pending" || !strings.Contains(got.Status.Message, "kube-system/calico-node") {
		t.Fatalf("status = %+v", got.Status)
	}

	r.SiteNetwork = func(context.Context, platform.Class) error { return &site.UnsupportedError{Reason: "embedded flannel"} }
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(claim)}); err == nil {
		t.Fatal("an unsupported site network was not reported")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), got); err != nil || got.Status.Phase != "Failed" {
		t.Fatalf("status = %+v, %v; want Failed", got.Status, err)
	}

	r.SiteNetwork = func(context.Context, platform.Class) error { return nil }
	if err := reconcileClaim(t, r, claim); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "native"}, machine); err != nil {
		t.Fatalf("no AWSMachine once the site network excludes it: %v", err)
	}
}

// selfAdmitting answers every security group as allowing all traffic from
// itself, as a VPC CNI node's groups must.
type selfAdmitting struct{}

func (selfAdmitting) Call(_ context.Context, _, _ string, input map[string]any) (json.RawMessage, error) {
	var groups []map[string]any
	for _, id := range input["GroupIds"].([]string) {
		groups = append(groups, map[string]any{"GroupId": id, "IpPermissions": []any{
			map[string]any{"IpProtocol": "-1", "UserIdGroupPairs": []any{map[string]any{"GroupId": id}}}}})
	}
	return json.Marshal(map[string]any{"SecurityGroups": groups})
}

func admittingGroups(context.Context, string) (attachmentaws.API, error) { return selfAdmitting{}, nil }

// separateGroups answers every security group as admitting nothing from
// the others.
type separateGroups struct{}

func (separateGroups) Call(_ context.Context, _, _ string, input map[string]any) (json.RawMessage, error) {
	var groups []map[string]any
	for _, id := range input["GroupIds"].([]string) {
		groups = append(groups, map[string]any{"GroupId": id})
	}
	return json.Marshal(map[string]any{"SecurityGroups": groups})
}

// A VPC CNI claim whose security groups do not admit each other fails with
// the reason, and no AWSMachine is created.
func TestReconcile_VPCCNIMachinesNeedGroupsThatAdmitEachOther(t *testing.T) {
	claim := fakeClaim("isolated")
	r := newClaimReconciler(t, claim, fakeTemplate("isolated"), fakeCluster("appmana"), fakeNode())
	r.Provisioners = []join.MachineProvisioner{joinaws.Provider{ConfigNamespace: "wg-dialer", ConfigName: "aws-provider-config",
		EC2: func(context.Context, string) (attachmentaws.API, error) { return separateGroups{}, nil }}}
	if err := reconcileClaim(t, r, claim); err == nil {
		t.Fatal("a VPC CNI claim with separate security groups was admitted")
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(joinaws.Provider{}.GVK())
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "isolated"}, machine); !apierrors.IsNotFound(err) {
		t.Fatalf("an AWSMachine exists: %v", err)
	}
	got := &v1alpha1.ProvisionedNodeClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), got); err != nil || got.Status.Phase != "Failed" || !strings.Contains(got.Status.Message, "sg-000000000000000cd") {
		t.Fatalf("status = %+v, %v", got.Status, err)
	}
}

func TestReconcile_MultiCAPI_AWSAndGCPInSameNamespace(t *testing.T) {
	awsClaim := fakeClaim("aws-worker")
	gcpClaim := &v1alpha1.ProvisionedNodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "gcp-worker", Namespace: "default", UID: "claim-gcp-1"},
		Spec: v1alpha1.ProvisionedNodeClaimSpec{
			InfrastructureRef: corev1.TypedLocalObjectReference{APIGroup: &infraGroup, Kind: "GCPMachineTemplate", Name: "gcp-worker"},
		},
	}
	gcpTemplate := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
		"kind":       "GCPMachineTemplate",
		"metadata":   map[string]any{"name": "gcp-worker", "namespace": "default"},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{"instanceType": "e2-standard-4"},
			},
		},
	}}
	gcpCluster := fakeCluster("appmana-gcp")
	_ = unstructured.SetNestedField(gcpCluster.Object, "GCPCluster", "spec", "infrastructureRef", "kind")

	azureClaim := &v1alpha1.ProvisionedNodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "azure-worker", Namespace: "default", UID: "claim-azure-1"},
		Spec: v1alpha1.ProvisionedNodeClaimSpec{
			InfrastructureRef: corev1.TypedLocalObjectReference{APIGroup: &infraGroup, Kind: "AzureMachineTemplate", Name: "azure-worker"},
		},
	}
	azureTemplate := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
		"kind":       "AzureMachineTemplate",
		"metadata":   map[string]any{"name": "azure-worker", "namespace": "default"},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{"vmSize": "Standard_D4s_v5"},
			},
		},
	}}
	azureCluster := fakeCluster("appmana-azure")
	_ = unstructured.SetNestedField(azureCluster.Object, "AzureCluster", "spec", "infrastructureRef", "kind")

	r := newClaimReconciler(t, awsClaim, gcpClaim, azureClaim, fakeTemplate("aws-worker"), gcpTemplate, azureTemplate, fakeCluster("appmana-aws"), gcpCluster, azureCluster, awsConfigSecret(), fakeNode())
	r.Provisioners = append(r.Provisioners, joingcp.Provider{}, joingeneric.Provider{})

	if err := reconcileClaim(t, r, awsClaim); err != nil {
		t.Fatalf("reconciling AWS claim in multi-CAPI namespace: %v", err)
	}
	if err := reconcileClaim(t, r, gcpClaim); err != nil {
		t.Fatalf("reconciling GCP claim in multi-CAPI namespace: %v", err)
	}
	if err := reconcileClaim(t, r, azureClaim); err != nil {
		t.Fatalf("reconciling generic CAPI (Azure) claim in multi-CAPI namespace: %v", err)
	}

	awsMachine := &unstructured.Unstructured{}
	awsMachine.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "aws-worker"}, awsMachine); err != nil {
		t.Fatalf("expected aws-worker Machine: %v", err)
	}
	if got, _, _ := unstructured.NestedString(awsMachine.Object, "spec", "clusterName"); got != "appmana-aws" {
		t.Fatalf("aws-worker clusterName = %q, want appmana-aws", got)
	}

	gcpMachine := &unstructured.Unstructured{}
	gcpMachine.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gcp-worker"}, gcpMachine); err != nil {
		t.Fatalf("expected gcp-worker Machine: %v", err)
	}
	if got, _, _ := unstructured.NestedString(gcpMachine.Object, "spec", "clusterName"); got != "appmana-gcp" {
		t.Fatalf("gcp-worker clusterName = %q, want appmana-gcp", got)
	}

	azureMachine := &unstructured.Unstructured{}
	azureMachine.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "azure-worker"}, azureMachine); err != nil {
		t.Fatalf("expected azure-worker Machine: %v", err)
	}
	if got, _, _ := unstructured.NestedString(azureMachine.Object, "spec", "clusterName"); got != "appmana-azure" {
		t.Fatalf("azure-worker clusterName = %q, want appmana-azure", got)
	}
	if got, _, _ := unstructured.NestedString(azureMachine.Object, "spec", "infrastructureRef", "kind"); got != "AzureMachine" {
		t.Fatalf("azure-worker infrastructureRef.kind = %q, want AzureMachine", got)
	}
}

type staticJoinProvider struct{}

func (staticJoinProvider) JoinValues(_ context.Context) (map[string]any, error) {
	return map[string]any{
		"apiEndpoint": "10.96.0.1:6443",
		"joinToken":   "test-k0s-join-token",
		"k0sVersion":  "v1.34.0+k0s.0",
	}, nil
}

type cloudConfigDoc struct {
	WriteFiles []struct {
		Path     string `json:"path"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	} `json:"write_files"`
	RunCmd []string `json:"runcmd"`
}

func TestExamplesMixedCloudWorkersManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "examples", "mixed-cloud-workers.yaml"))
	if err != nil {
		t.Fatalf("reading examples/mixed-cloud-workers.yaml: %v", err)
	}
	var initObjects []client.Object
	var claims []*v1alpha1.ProvisionedNodeClaim
	for _, doc := range strings.Split(string(raw), "\n---") {
		var u unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			t.Fatalf("unmarshaling YAML doc: %v", err)
		}
		if len(u.Object) == 0 || u.GetKind() == "" {
			continue
		}
		if u.GetKind() == "ProvisionedNodeClaim" {
			var c v1alpha1.ProvisionedNodeClaim
			if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
				t.Fatalf("unmarshaling ProvisionedNodeClaim: %v", err)
			}
			c.UID = types.UID("uid-" + c.Name)
			initObjects = append(initObjects, &c)
			claims = append(claims, &c)
			continue
		}
		initObjects = append(initObjects, &u)
	}
	peerSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tunnel-peers", Namespace: "cloud-provisioning"},
		Data: map[string][]byte{
			"node-public-key-onprem-0":     []byte("c3BhcmsyYWIzLXB1YmxpYy1rZXktdGVzdC1vbmx5PT0="),
			"node-tunnel-address-onprem-0": []byte("10.100.0.1/24"),
			"node-addresses-onprem-0":      []byte("192.0.2.10"),
		},
	}
	initObjects = append(initObjects, awsConfigSecret(), fakeNode(), peerSecret)

	r := newClaimReconciler(t, initObjects...)
	r.Provisioners = append(r.Provisioners, joingcp.Provider{}, joingeneric.Provider{})

	if len(claims) != 4 {
		t.Fatalf("expected 4 ProvisionedNodeClaims in examples/mixed-cloud-workers.yaml, got %d", len(claims))
	}
	for _, c := range claims {
		if err := reconcileClaim(t, r, c); err != nil {
			t.Fatalf("reconciling %s: %v", c.Name, err)
		}
	}

	joinReconciler := &join.Reconciler{
		Client: r.Client,
		Reader: r.Reader,
		Join:   staticJoinProvider{},
		InfraProviders: []join.InfraProvider{
			joinaws.Provider{ConfigNamespace: "wg-dialer", ConfigName: "aws-provider-config"},
			joingcp.Provider{},
			joingeneric.Provider{},
		},
		BootstrapRenderers: map[string]join.BootstrapRenderer{
			"linux": join.PatternBootstrap{
				Path:   filepath.Join("..", "..", "..", "join-patterns", "k0s-worker.cloud-config.tmpl"),
				Format: bootstrap.CloudConfig,
			},
		},
		APIVIP:                    "10.96.0.1",
		WireGuardAddress:          "10.100.0.2/24",
		WireGuardListenPort:       "51820",
		DialerPeerSecretNamespace: "cloud-provisioning",
		DialerPeerSecretName:      "tunnel-peers",
		DialerListenPort:          "51820",
		InterfaceName:             "cldt0",
		DialerBinaryURLARM64:      "https://example.invalid/wg-dialer-arm64",
		DialerBinarySHA256ARM64:   "1111111111111111111111111111111111111111111111111111111111111111",
		DialerBinaryURLAMD64:      "https://example.invalid/wg-dialer-amd64",
		DialerBinarySHA256AMD64:   "2222222222222222222222222222222222222222222222222222222222222222",
		CNIPluginsURLARM64:        "https://example.invalid/cni-arm64.tgz",
		CNIPluginsSHA256ARM64:     "3333333333333333333333333333333333333333333333333333333333333333",
		CNIPluginsURLAMD64:        "https://example.invalid/cni-amd64.tgz",
		CNIPluginsSHA256AMD64:     "4444444444444444444444444444444444444444444444444444444444444444",
		BootstrapSecretNameFormat: "%s-bootstrap",
	}

	orderedNames := []string{"aws-worker-1", "aws-worker-2", "gcp-worker-1", "azure-worker-1"}
	wantMachines := map[string]struct {
		clusterName string
		infraKind   string
		tunnelAddr  string
		cloud       string
		wantID      string
		wantNodeIP  string
	}{
		"aws-worker-1": {
			clusterName: "site-aws",
			infraKind:   "AWSMachine",
			tunnelAddr:  "10.100.0.2/24",
			cloud:       "aws",
			wantID:      "--provider-id=aws:///us-west-2a/i-0123456789abcdef0",
			wantNodeIP:  "--node-ip=10.0.1.10,2600:1f14::10",
		},
		"aws-worker-2": {
			clusterName: "site-aws",
			infraKind:   "AWSMachine",
			tunnelAddr:  "10.100.0.3/24",
			cloud:       "aws",
			wantID:      "--provider-id=aws:///us-west-2a/i-0123456789abcdef0",
			wantNodeIP:  "--node-ip=10.0.1.10,2600:1f14::10",
		},
		"gcp-worker-1": {
			clusterName: "site-gcp",
			infraKind:   "GCPMachine",
			tunnelAddr:  "10.100.0.4/24",
			cloud:       "gcp",
			wantID:      "--provider-id=gce://my-gcp-project/us-central1-a/gcp-worker-1",
			wantNodeIP:  "--node-ip=10.128.0.5",
		},
		"azure-worker-1": {
			clusterName: "site-azure",
			infraKind:   "AzureMachine",
			tunnelAddr:  "10.100.0.5/24",
			cloud:       "azure",
			wantID:      "--provider-id=azure:///subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/azure-worker-1",
			wantNodeIP:  "--node-ip=10.2.0.4",
		},
	}

	seenPrivateKeys := map[string]string{}
	for _, name := range orderedNames {
		want := wantMachines[name]
		m := &unstructured.Unstructured{}
		m.SetGroupVersionKind(machineGVK)
		if err := r.Get(context.Background(), client.ObjectKey{Namespace: "cloud-provisioning", Name: name}, m); err != nil {
			t.Fatalf("expected Machine %s: %v", name, err)
		}
		if got, _, _ := unstructured.NestedString(m.Object, "spec", "clusterName"); got != want.clusterName {
			t.Fatalf("%s clusterName = %q, want %q", name, got, want.clusterName)
		}
		if got, _, _ := unstructured.NestedString(m.Object, "spec", "infrastructureRef", "kind"); got != want.infraKind {
			t.Fatalf("%s infrastructureRef.kind = %q, want %q", name, got, want.infraKind)
		}

		if m.GetUID() == "" {
			m.SetUID(types.UID("machine-" + name))
			if err := r.Update(context.Background(), m); err != nil {
				t.Fatalf("setting UID on Machine %s: %v", name, err)
			}
		}

		// Run join.Reconciler to render the #cloud-config bootstrap Secret and register the WireGuard peer.
		if _, err := joinReconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: "cloud-provisioning", Name: name},
		}); err != nil {
			t.Fatalf("join.Reconcile(%s): %v", name, err)
		}

		bootstrapSecret := &corev1.Secret{}
		if err := r.Get(context.Background(), client.ObjectKey{Namespace: "cloud-provisioning", Name: name + "-bootstrap"}, bootstrapSecret); err != nil {
			t.Fatalf("expected bootstrap Secret %s-bootstrap: %v", name, err)
		}
		format := string(bootstrapSecret.Data["format"])
		if format == "" {
			format = bootstrapSecret.StringData["format"]
		}
		if format != bootstrap.CloudConfig {
			t.Fatalf("%s-bootstrap format = %q, want %q", name, format, bootstrap.CloudConfig)
		}
		userdata := string(bootstrapSecret.Data["value"])
		if userdata == "" {
			userdata = bootstrapSecret.StringData["value"]
		}
		if !strings.HasPrefix(userdata, "#cloud-config\n") {
			t.Fatalf("%s-bootstrap does not start with #cloud-config", name)
		}

		var doc cloudConfigDoc
		if err := yaml.Unmarshal([]byte(userdata), &doc); err != nil {
			t.Fatalf("unmarshaling %s-bootstrap cloud-config YAML: %v", name, err)
		}

		filesByPath := map[string]string{}
		for _, wf := range doc.WriteFiles {
			content := wf.Content
			if wf.Encoding == "b64" {
				decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(wf.Content))
				if err != nil {
					t.Fatalf("%s-bootstrap decoding b64 file %s: %v", name, wf.Path, err)
				}
				content = string(decoded)
			}
			filesByPath[wf.Path] = content
		}

		if gotName := strings.TrimSpace(filesByPath["/etc/wg-dialer/machine-name"]); gotName != name {
			t.Fatalf("%s /etc/wg-dialer/machine-name = %q, want %q", name, gotName, name)
		}
		if strings.Contains(filesByPath["/etc/systemd/system/wg-dialer.service"], "--mtu=1420") {
			t.Fatalf("%s wg-dialer.service must not hardcode --mtu=1420 (breaks GCP 1460 MTU VPCs)", name)
		}
		if !strings.Contains(filesByPath["/etc/NetworkManager/conf.d/90-cloud-provisioning.conf"], "unmanaged-devices=") {
			t.Fatalf("%s missing NetworkManager unmanaged-devices config", name)
		}
		if len(doc.RunCmd) == 0 || !strings.Contains(doc.RunCmd[0], "apt-get") || !strings.Contains(doc.RunCmd[0], "dnf") || !strings.Contains(doc.RunCmd[0], "yum") {
			t.Fatalf("%s first runcmd does not contain cross-distro package bootstrap: %v", name, doc.RunCmd)
		}

		var peersDoc tunnel.PeersFileDoc
		if err := json.Unmarshal([]byte(filesByPath["/etc/wg-dialer/peers.json"]), &peersDoc); err != nil {
			t.Fatalf("%s unmarshaling /etc/wg-dialer/peers.json: %v", name, err)
		}
		if peersDoc.LocalAddress != want.tunnelAddr {
			t.Fatalf("%s peers.json LocalAddress = %q, want %q", name, peersDoc.LocalAddress, want.tunnelAddr)
		}
		if peersDoc.PrivateKey == "" {
			t.Fatalf("%s peers.json PrivateKey is empty", name)
		}
		if prevMachine, dup := seenPrivateKeys[peersDoc.PrivateKey]; dup {
			t.Fatalf("%s reused WireGuard PrivateKey from %s", name, prevMachine)
		}
		seenPrivateKeys[peersDoc.PrivateKey] = name

		// Execute the rendered cloud-provider-identity script and the rendered
		// runcmd identity/node-IP discovery block in sh -e against simulated
		// cloud metadata endpoints to verify real guest execution.
		executeRenderedCloudInitDiscovery(t, name, want.cloud, filesByPath["/usr/local/bin/cloud-provider-identity"], doc.RunCmd, want.wantID, want.wantNodeIP)
		executeRenderedCrossDistroBootstrap(t, name, doc.RunCmd[0])
	}

	// Verify CAPG & CAPZ infrastructure specializations on the expanded objects.
	gcpCluster := &unstructured.Unstructured{}
	gcpCluster.SetGroupVersionKind(schema.GroupVersionKind{Group: infraGroup, Version: "v1beta1", Kind: "GCPCluster"})
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "cloud-provisioning", Name: "site-gcp"}, gcpCluster); err != nil {
		t.Fatalf("getting GCPCluster site-gcp: %v", err)
	}
	fds, _, _ := unstructured.NestedMap(gcpCluster.Object, "status", "failureDomains")
	if _, ok := fds["us-central1-a"]; !ok {
		t.Fatalf("GCPCluster status.failureDomains = %+v, want us-central1-a populated for external CAPG cluster", fds)
	}

	gcpMachine := &unstructured.Unstructured{}
	gcpMachine.SetGroupVersionKind(schema.GroupVersionKind{Group: infraGroup, Version: "v1beta1", Kind: "GCPMachine"})
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "cloud-provisioning", Name: "gcp-worker-1"}, gcpMachine); err != nil {
		t.Fatalf("getting GCPMachine gcp-worker-1: %v", err)
	}
	if pub, _, _ := unstructured.NestedBool(gcpMachine.Object, "spec", "publicIP"); !pub {
		t.Fatal("GCPMachine spec.publicIP must default to true so GCE VM gets ONE_TO_ONE_NAT ExternalIP")
	}
	metaSlice, _, _ := unstructured.NestedSlice(gcpMachine.Object, "spec", "additionalMetadata")
	if len(metaSlice) == 0 {
		t.Fatal("GCPMachine spec.additionalMetadata must contain startup-script cloud-init bridge for Debian-12/RHEL")
	}

	gcpCapiMachine := &unstructured.Unstructured{}
	gcpCapiMachine.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "cloud-provisioning", Name: "gcp-worker-1"}, gcpCapiMachine); err != nil {
		t.Fatalf("getting CAPI Machine gcp-worker-1: %v", err)
	}
	if fd, _, _ := unstructured.NestedString(gcpCapiMachine.Object, "spec", "failureDomain"); fd != "us-central1-a" {
		t.Fatalf("CAPI Machine gcp-worker-1 spec.failureDomain = %q, want us-central1-a", fd)
	}

	azureMachine := &unstructured.Unstructured{}
	azureMachine.SetGroupVersionKind(schema.GroupVersionKind{Group: infraGroup, Version: "v1beta1", Kind: "AzureMachine"})
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "cloud-provisioning", Name: "azure-worker-1"}, azureMachine); err != nil {
		t.Fatalf("getting AzureMachine azure-worker-1: %v", err)
	}
	if allocPub, _, _ := unstructured.NestedBool(azureMachine.Object, "spec", "allocatePublicIP"); !allocPub {
		t.Fatal("AzureMachine spec.allocatePublicIP must default to true on external AzureCluster")
	}
}

func executeRenderedCloudInitDiscovery(
	t *testing.T,
	machineName string,
	cloud string,
	identityScript string,
	runcmds []string,
	wantProviderIDArg string,
	wantNodeIPArg string,
) {
	t.Helper()
	if identityScript == "" {
		t.Fatalf("%s missing /usr/local/bin/cloud-provider-identity in write_files", machineName)
	}
	var joinCmd string
	for _, cmd := range runcmds {
		if strings.Contains(cmd, "PROVIDER_ID_ARG=") && strings.Contains(cmd, "NODE_IP_ARG=") {
			joinCmd = cmd
			break
		}
	}
	if joinCmd == "" {
		t.Fatalf("%s missing runcmd containing PROVIDER_ID_ARG and NODE_IP_ARG", machineName)
	}
	tmpDir := t.TempDir()
	idPath := filepath.Join(tmpDir, "cloud-provider-identity")
	if err := os.WriteFile(idPath, []byte(identityScript), 0o755); err != nil {
		t.Fatalf("writing cloud-provider-identity: %v", err)
	}
	discoveryShell := strings.ReplaceAll(joinCmd, "/usr/local/bin/cloud-provider-identity", idPath)

	mockK0s := filepath.Join(tmpDir, "k0s")
	k0sScript := "#!/bin/sh\nprintf 'K0S_INVOCATION=%s\\n' \"$*\"\n"
	if err := os.WriteFile(mockK0s, []byte(k0sScript), 0o755); err != nil {
		t.Fatalf("writing mock k0s: %v", err)
	}

	mockCurl := filepath.Join(tmpDir, "curl")
	curlScript := `#!/bin/sh
url=""
for arg; do
  case "$arg" in
    http://*|https://*) url="$arg" ;;
  esac
done
case "$CLOUD_SIM:$url" in
  aws:http://169.254.169.254/latest/api/token)
    printf 'aws-imds-token'
    exit 0
    ;;
  aws:http://169.254.169.254/latest/meta-data/placement/availability-zone)
    printf 'us-west-2a'
    exit 0
    ;;
  aws:http://169.254.169.254/latest/meta-data/instance-id)
    printf 'i-0123456789abcdef0'
    exit 0
    ;;
  aws:http://169.254.169.254/latest/meta-data/local-ipv4)
    printf '10.0.1.10'
    exit 0
    ;;
  aws:http://169.254.169.254/latest/meta-data/ipv6)
    printf '2600:1f14::10'
    exit 0
    ;;
  gcp:http://metadata.google.internal/computeMetadata/v1/project/project-id)
    printf 'my-gcp-project'
    exit 0
    ;;
  gcp:http://metadata.google.internal/computeMetadata/v1/instance/zone)
    printf 'projects/123456/zones/us-central1-a'
    exit 0
    ;;
  gcp:http://metadata.google.internal/computeMetadata/v1/instance/name)
    printf 'gcp-worker-1'
    exit 0
    ;;
  gcp:http://metadata.google.internal/computeMetadata/v1/instance/network-interfaces/0/ip)
    printf '10.128.0.5'
    exit 0
    ;;
  azure:http://169.254.169.254/metadata/instance/compute/resourceId*)
    printf '/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/azure-worker-1'
    exit 0
    ;;
  azure:http://169.254.169.254/metadata/instance/network/interface/0/ipv4/ipAddress/0/privateIpAddress*)
    printf '10.2.0.4'
    exit 0
    ;;
esac
exit 22
`
	if err := os.WriteFile(mockCurl, []byte(curlScript), 0o755); err != nil {
		t.Fatalf("writing mock curl: %v", err)
	}

	cmd := exec.Command("sh", "-e", "-c", discoveryShell)
	cmd.Env = append(os.Environ(),
		"PATH="+tmpDir+":"+os.Getenv("PATH"),
		"CLOUD_SIM="+cloud,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s executing rendered cloud-config discovery shell in sh -e failed: %v\noutput:\n%s\nshell:\n%s",
			machineName, err, string(out), discoveryShell)
	}
	got := string(out)
	if !strings.Contains(got, wantProviderIDArg) {
		t.Fatalf("%s rendered shell output:\n%s\nwant %s in k0s invocation", machineName, got, wantProviderIDArg)
	}
	if !strings.Contains(got, wantNodeIPArg) {
		t.Fatalf("%s rendered shell output:\n%s\nwant %s in k0s invocation", machineName, got, wantNodeIPArg)
	}
}

func executeRenderedCrossDistroBootstrap(t *testing.T, machineName string, dialerInstallCmd string) {
	t.Helper()
	idx := strings.Index(dialerInstallCmd, `case "$(uname -m)" in`)
	if idx <= 0 {
		t.Fatalf("%s dialer-install runcmd missing uname arch switch: %s", machineName, dialerInstallCmd)
	}
	pkgBootstrapCmd := dialerInstallCmd[:idx]

	cases := []struct {
		name        string
		pkgMgr      string
		preExisting []string
		wantSubstr  string
		forbidSub   string
	}{
		{
			name:        "debian-ubuntu-apt",
			pkgMgr:      "apt-get",
			preExisting: []string{"tar", "gzip", "ip"},
			wantSubstr:  "APT_GET=install -y -q ca-certificates curl tar gzip iptables conntrack socat iproute2",
		},
		{
			name:        "amazon-linux-2023-dnf-with-curl-minimal",
			pkgMgr:      "dnf",
			preExisting: []string{"curl", "tar", "gzip", "ip"},
			wantSubstr:  "DNF=install -y -q ca-certificates tar gzip iptables conntrack-tools socat iproute",
			forbidSub:   "curl",
		},
		{
			name:        "rhel-yum-minimal",
			pkgMgr:      "yum",
			preExisting: []string{"tar", "gzip", "ip"},
			wantSubstr:  "YUM=install -y -q ca-certificates tar gzip iptables conntrack-tools socat iproute curl",
		},
		{
			name:        "sles-zypper",
			pkgMgr:      "zypper",
			preExisting: []string{"curl", "tar", "gzip", "ip"},
			wantSubstr:  "ZYPPER=--non-interactive install -y ca-certificates curl tar gzip iptables conntrack-tools socat iproute2",
		},
	}

	for _, tc := range cases {
		binDir := t.TempDir()
		for _, bin := range tc.preExisting {
			p := filepath.Join(binDir, bin)
			if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatalf("writing mock %s: %v", bin, err)
			}
		}
		mgrScript := "#!/bin/sh\nprintf '" + strings.ToUpper(strings.ReplaceAll(tc.pkgMgr, "-", "_")) + "=%s\\n' \"$*\"\n"
		if err := os.WriteFile(filepath.Join(binDir, tc.pkgMgr), []byte(mgrScript), 0o755); err != nil {
			t.Fatalf("writing mock %s: %v", tc.pkgMgr, err)
		}

		cmd := exec.Command("/bin/sh", "-e", "-c", pkgBootstrapCmd)
		cmd.Env = []string{"PATH=" + binDir}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s (%s) cross-distro bootstrap failed: %v\noutput:\n%s", machineName, tc.name, err, string(out))
		}
		got := string(out)
		if !strings.Contains(got, tc.wantSubstr) {
			t.Fatalf("%s (%s) output = %q, want substring %q", machineName, tc.name, got, tc.wantSubstr)
		}
		if tc.forbidSub != "" && strings.Contains(got, tc.forbidSub) {
			t.Fatalf("%s (%s) output = %q, must not contain %q (AL2023 curl-minimal conflict)", machineName, tc.name, got, tc.forbidSub)
		}
	}
}
