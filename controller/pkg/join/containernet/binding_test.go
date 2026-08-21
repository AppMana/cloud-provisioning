package containernet

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The machine says which container backs it, in its own spec.
//
// That is where a template puts it: a ContainernetMachineTemplate
// carries spec.template.spec.containerName, and the claim reconciler
// copies that template's spec into the machine it creates — the same
// way an AWSMachineTemplate's instanceType reaches an AWSMachine.
// Reading only an annotation left that field dead and made the
// harness set the binding twice, once where the API says it goes and
// once where the code actually looked.
func TestTheMachineSpecSaysWhichContainerBacksIt(t *testing.T) {
	machine := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "public-worker"},
		"spec":     map[string]any{"containerName": "clab-cldt-remote1"},
	}}
	if got := containerName(machine); got != "clab-cldt-remote1" {
		t.Errorf("containerName = %q, want the spec's own field", got)
	}
}

// The annotation still wins when it is set, because it is what the
// printer column shows and what already-created objects carry.
func TestTheAnnotationOverridesTheSpec(t *testing.T) {
	machine := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name":        "public-worker",
			"annotations": map[string]any{containerNameAnnotation: "clab-cldt-remote2"},
		},
		"spec": map[string]any{"containerName": "clab-cldt-remote1"},
	}}
	if got := containerName(machine); got != "clab-cldt-remote2" {
		t.Errorf("containerName = %q, want the annotation to win", got)
	}
}

// With neither, the object's own name is the last resort: a machine
// and a container may share a name, and guessing that is better than
// returning nothing at all.
func TestWithNoBindingTheObjectsNameIsUsed(t *testing.T) {
	machine := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "public-worker"},
	}}
	if got := containerName(machine); got != "public-worker" {
		t.Errorf("containerName = %q, want the object's name", got)
	}
}
