package claim

import (
	"context"
	"testing"

	v1alpha1 "github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	joinlabcontainers "github.com/appmana/cloud-provisioning/controller/pkg/join/labcontainers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A provider serves its templates at its own API version. Labcontainers'
// CAPI contract is v1alpha1, and reading every template at v1beta2
// failed each lab claim with "no matches for kind LabMachineTemplate in
// version infrastructure.labcontainers.appmana.com/v1beta2" before any
// machine existed. The template and the machine are read and written at
// the version the API serves for them.
func TestReconcile_TemplateVersionIsTheOneTheAPIServes(t *testing.T) {
	provider := joinlabcontainers.Provider{}
	machineGVK, clusterGVK := provider.GVK(), provider.ClusterGVK()
	templateGVK := machineGVK.GroupVersion().WithKind(machineGVK.Kind + "Template")
	scheme := testScheme(t)
	for _, kind := range []string{machineGVK.Kind, clusterGVK.Kind, templateGVK.Kind} {
		scheme.AddKnownTypeWithName(machineGVK.GroupVersion().WithKind(kind), &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(machineGVK.GroupVersion().WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	group := machineGVK.Group
	claim := fakeClaim("remote1")
	claim.Spec.InfrastructureRef = corev1.TypedLocalObjectReference{APIGroup: &group, Kind: templateGVK.Kind, Name: "remote1"}
	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(templateGVK)
	template.SetName("remote1")
	template.SetNamespace("default")
	_ = unstructured.SetNestedMap(template.Object, map[string]any{"slot": "remote1"}, "spec", "template", "spec")
	cluster := fakeCluster("lab")
	_ = unstructured.SetNestedField(cluster.Object, clusterGVK.Kind, "spec", "infrastructureRef", "kind")
	labCluster := &unstructured.Unstructured{}
	labCluster.SetGroupVersionKind(clusterGVK)
	labCluster.SetName("lab")
	labCluster.SetNamespace("default")
	_ = unstructured.SetNestedField(labCluster.Object, true, "status", "initialization", "provisioned")
	_ = unstructured.SetNestedField(labCluster.Object, true, "status", "ready")

	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(servedBy(scheme)).
		WithStatusSubresource(&v1alpha1.ProvisionedNodeClaim{}).
		WithObjects(claim, template, cluster, labCluster, fakeNode()).Build()
	r := &Reconciler{
		Client: c, Reader: c,
		Provisioners:              []join.MachineProvisioner{provider},
		RoleLabel:                 "cloud-provisioning.appmana.com/role",
		RoleValue:                 "cloud-worker",
		BootstrapSecretNameFormat: "%s-bootstrap",
		TunnelInterface:           "cldt0a1b2c3d",
	}
	if err := reconcileClaim(t, r, claim); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "remote1"}, machine); err != nil {
		t.Fatalf("no %s at %s: %v", machineGVK.Kind, machineGVK.GroupVersion(), err)
	}
	if slot, _, _ := unstructured.NestedString(machine.Object, "spec", "slot"); slot != "remote1" {
		t.Errorf("machine spec = %v, want the template's", machine.Object["spec"])
	}
	got := &v1alpha1.ProvisionedNodeClaim{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(claim), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == "Failed" {
		t.Errorf("claim failed: %s", got.Status.Message)
	}
}

// An externally managed Cluster's infrastructure object is marked ready by
// this controller, at the version its provider serves.
func TestExternallyManagedInfrastructureIsReadAtItsServedVersion(t *testing.T) {
	provider := joinlabcontainers.Provider{}
	clusterGVK := provider.ClusterGVK()
	scheme := testScheme(t)
	scheme.AddKnownTypeWithName(clusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(clusterGVK.GroupVersion().WithKind(clusterGVK.Kind+"List"), &unstructured.UnstructuredList{})
	cluster := fakeCluster("lab")
	cluster.SetAnnotations(map[string]string{"cluster.x-k8s.io/managed-by": "external"})
	_ = unstructured.SetNestedField(cluster.Object, clusterGVK.Kind, "spec", "infrastructureRef", "kind")
	_ = unstructured.SetNestedField(cluster.Object, clusterGVK.Group, "spec", "infrastructureRef", "apiGroup")
	labCluster := &unstructured.Unstructured{}
	labCluster.SetGroupVersionKind(clusterGVK)
	labCluster.SetName("lab")
	labCluster.SetNamespace("default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(servedBy(scheme)).
		WithStatusSubresource(cluster, labCluster).WithObjects(cluster, labCluster).Build()
	r := &Reconciler{Client: c, Reader: c}
	if err := r.ensureClusterProvisioned(context.Background(), cluster); err != nil {
		t.Fatalf("ensureClusterProvisioned: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(clusterGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "lab"}, got); err != nil {
		t.Fatal(err)
	}
	if ready, _, _ := unstructured.NestedBool(got.Object, "status", "ready"); !ready {
		t.Error("the infrastructure cluster was not marked ready")
	}
}
