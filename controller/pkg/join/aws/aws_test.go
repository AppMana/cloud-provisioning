package aws

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
