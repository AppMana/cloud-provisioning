package aws

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Provider never calls the AWS API itself -- it only reads
// status.ready off whatever CAPA already wrote into the AWSMachine
// object. So these tests exercise exactly that parsing logic against
// fake AWSMachine shapes; they deliberately do NOT touch real AWS or
// CAPA (that's a live-cluster/E2E concern, not a unit-test one -- see
// the jarvistam-cloud-worker-0 AWSMachine actually reconciling live).
func fakeAWSMachine(t *testing.T, fields map[string]any) *unstructured.Unstructured {
	t.Helper()
	m := &unstructured.Unstructured{Object: map[string]any{}}
	for path, val := range fields {
		_ = unstructured.SetNestedField(m.Object, val, "status", path)
	}
	return m
}

func TestReady_StatusReadyTrue(t *testing.T) {
	m := fakeAWSMachine(t, map[string]any{"ready": true})
	ready, err := Provider{}.Ready(context.Background(), m)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !ready {
		t.Error("Ready = false, want true when status.ready=true")
	}
}

func TestReady_StatusReadyFalse(t *testing.T) {
	m := fakeAWSMachine(t, map[string]any{"ready": false})
	ready, err := Provider{}.Ready(context.Background(), m)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready {
		t.Error("Ready = true, want false when status.ready=false")
	}
}

func TestReady_StatusReadyMissing(t *testing.T) {
	// Before CAPA has reconciled the AWSMachine at all, status.ready
	// simply doesn't exist yet -- must be treated as "not ready", not
	// an error (the reconciler requeues quietly on false, nil).
	m := &unstructured.Unstructured{Object: map[string]any{}}
	ready, err := Provider{}.Ready(context.Background(), m)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready {
		t.Error("Ready = true for an AWSMachine with no status at all")
	}
}

func TestReady_StatusReadyWrongType(t *testing.T) {
	// A real API server would never let status.ready be a string given
	// AWSMachine's CRD schema, but this proves malformed input is
	// surfaced as a genuine error rather than silently misread as
	// falsy.
	m := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"ready": "true"},
	}}
	if _, err := (Provider{}).Ready(context.Background(), m); err == nil {
		t.Fatal("expected an error when status.ready isn't a bool, got nil")
	}
}

func TestInfraValues_ReturnsEmptyNonNilMap(t *testing.T) {
	m := fakeAWSMachine(t, map[string]any{"ready": true})
	values, err := Provider{}.InfraValues(context.Background(), m)
	if err != nil {
		t.Fatalf("InfraValues: %v", err)
	}
	if values == nil {
		t.Error("InfraValues returned nil, want an empty-but-non-nil map")
	}
	if len(values) != 0 {
		t.Errorf("InfraValues = %v, want empty (AWS contributes nothing today)", values)
	}
}

// validateFixtures builds the full Cluster -> AWSCluster ->
// AWSClusterStaticIdentity chain Validate traces, mirroring the real
// live objects (hilton-jarvistam / jarvistam-cloud-worker) this check
// was written against. secretNamespace lets a test place the
// credentials Secret in the wrong place, matching the actual live bug
// caught: the Secret existed only in "default", not "capa-system"
// (CAPA's own manager namespace), and CAPA retried forever with an
// opaque "Secret ... not found".
func validateFixtures(t *testing.T, secretNamespace string) (client.Client, *unstructured.Unstructured) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	scheme.AddKnownTypeWithName(clusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(awsClusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(awsClusterStaticIdentityGVK, &unstructured.Unstructured{})

	awsMachine := &unstructured.Unstructured{}
	awsMachine.SetGroupVersionKind(gvk)
	awsMachine.SetName("hilton-cloud-worker-jarvistam-0")
	awsMachine.SetNamespace("default")
	awsMachine.SetLabels(map[string]string{clusterNameLabel: "hilton-jarvistam"})

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetName("hilton-jarvistam")
	cluster.SetNamespace("default")
	_ = unstructured.SetNestedField(cluster.Object, "AWSCluster", "spec", "infrastructureRef", "kind")
	_ = unstructured.SetNestedField(cluster.Object, "hilton-jarvistam", "spec", "infrastructureRef", "name")

	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(awsClusterGVK)
	awsCluster.SetName("hilton-jarvistam")
	awsCluster.SetNamespace("default")
	_ = unstructured.SetNestedField(awsCluster.Object, "AWSClusterStaticIdentity", "spec", "identityRef", "kind")
	_ = unstructured.SetNestedField(awsCluster.Object, "jarvistam-cloud-worker", "spec", "identityRef", "name")

	identity := &unstructured.Unstructured{}
	identity.SetGroupVersionKind(awsClusterStaticIdentityGVK)
	identity.SetName("jarvistam-cloud-worker")
	_ = unstructured.SetNestedField(identity.Object, "jarvistam-cloud-worker-credentials", "spec", "secretRef")

	objs := []client.Object{cluster, awsCluster, identity}
	if secretNamespace != "" {
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "jarvistam-cloud-worker-credentials", Namespace: secretNamespace},
		})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return c, awsMachine
}

func TestValidate_SecretInWrongNamespace_ReturnsActionableError(t *testing.T) {
	// The exact live bug: the credentials Secret existed only in
	// "default" (the AWSCluster's own namespace), never in
	// "capa-system" (CAPA's manager namespace) -- CAPA's own error
	// ("Secret ... not found") gave no hint about WHERE it actually
	// needed to be.
	c, awsMachine := validateFixtures(t, "default")

	err := (Provider{}).Validate(context.Background(), c, awsMachine)
	if err == nil {
		t.Fatal("expected an error when the identity secret is in the wrong namespace, got nil")
	}
	for _, want := range []string{"jarvistam-cloud-worker-credentials", "capa-system", "default"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message %q missing %q -- must name the secret and both namespaces so it's actually actionable", err.Error(), want)
		}
	}
}

func TestValidate_SecretInManagerNamespace_ReturnsNil(t *testing.T) {
	c, awsMachine := validateFixtures(t, managerNamespace)

	if err := (Provider{}).Validate(context.Background(), c, awsMachine); err != nil {
		t.Errorf("Validate: %v, want nil when the secret is correctly placed in %s", err, managerNamespace)
	}
}

func TestValidate_SecretMissingEntirely_ReturnsError(t *testing.T) {
	c, awsMachine := validateFixtures(t, "")

	if err := (Provider{}).Validate(context.Background(), c, awsMachine); err == nil {
		t.Fatal("expected an error when the identity secret doesn't exist anywhere, got nil")
	}
}

func TestValidate_NoClusterNameLabel_ReturnsNil(t *testing.T) {
	// A brand new AWSMachine CAPI hasn't labeled yet -- nothing to
	// trace, not an error.
	awsMachine := &unstructured.Unstructured{}
	awsMachine.SetGroupVersionKind(gvk)
	awsMachine.SetName("some-machine")
	awsMachine.SetNamespace("default")

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	if err := (Provider{}).Validate(context.Background(), c, awsMachine); err != nil {
		t.Errorf("Validate: %v, want nil when there's no cluster-name label yet", err)
	}
}

func TestGVK_IsAWSMachineV1beta2(t *testing.T) {
	// v1beta2 is the storage version confirmed live against the real
	// hilton cluster's installed CRD (kubectl get crd
	// awsmachines.infrastructure.cluster.x-k8s.io -o
	// jsonpath={.spec.versions}), not a guess.
	gvk := Provider{}.GVK()
	if gvk.Group != "infrastructure.cluster.x-k8s.io" || gvk.Version != "v1beta2" || gvk.Kind != "AWSMachine" {
		t.Errorf("GVK = %+v, want infrastructure.cluster.x-k8s.io/v1beta2, Kind=AWSMachine", gvk)
	}
}
