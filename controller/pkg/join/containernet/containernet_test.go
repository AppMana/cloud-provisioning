package containernet

import (
	"context"
	"os/exec"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// requireDocker skips the test on any machine without a working Docker
// daemon (e.g. CI without docker-in-docker) rather than failing --
// these tests exercise a real `docker` binary, deliberately not mocked.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available, skipping containernet integration test")
	}
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skip("docker daemon not reachable, skipping containernet integration test")
	}
}

func fakeContainernetMachine(name string) *unstructured.Unstructured {
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(gvk)
	m.SetName(name)
	return m
}

func TestReady_NonexistentContainer_ReturnsFalseNotError(t *testing.T) {
	requireDocker(t)
	machine := fakeContainernetMachine("join-test-definitely-does-not-exist-abc123")

	ready, err := Provider{}.Ready(context.Background(), machine)
	if err != nil {
		t.Fatalf("Ready on a nonexistent container must report \"not ready\", not an error: %v", err)
	}
	if ready {
		t.Error("Ready = true for a container that was never created")
	}
}

func TestCreateMachine_ThenReady_ReflectsRealContainerState(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	const name = "join-test-containernet-machine"
	const image = "alpine:3.20"

	// Red: before creation, Ready must be false -- proves the green
	// result below reflects CreateMachine's real effect, not a stub
	// that always returns true.
	machine := fakeContainernetMachine(name)
	if ready, err := (Provider{}).Ready(ctx, machine); err != nil || ready {
		t.Fatalf("precondition failed: Ready() = (%v, %v) before CreateMachine, want (false, nil)", ready, err)
	}

	if err := CreateMachine(ctx, name, image); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	t.Cleanup(func() {
		if err := DestroyMachine(context.Background(), name); err != nil {
			t.Errorf("DestroyMachine cleanup: %v", err)
		}
	})

	// Green: a real container is now actually running.
	ready, err := Provider{}.Ready(ctx, machine)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !ready {
		t.Error("Ready = false after CreateMachine succeeded -- container should be running")
	}

	values, err := Provider{}.InfraValues(ctx, machine)
	if err != nil {
		t.Fatalf("InfraValues: %v", err)
	}
	if values == nil {
		t.Error("InfraValues returned nil, want an empty-but-non-nil map (matches aws.Provider's contract)")
	}
}

func TestReady_UsesContainerNameAnnotationWhenPresent(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	const containerName = "join-test-annotated-container-name"
	const image = "alpine:3.20"

	machine := fakeContainernetMachine("some-other-k8s-object-name")
	machine.SetAnnotations(map[string]string{containerNameAnnotation: containerName})

	if err := CreateMachine(ctx, containerName, image); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	t.Cleanup(func() {
		if err := DestroyMachine(context.Background(), containerName); err != nil {
			t.Errorf("DestroyMachine cleanup: %v", err)
		}
	})

	ready, err := Provider{}.Ready(ctx, machine)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !ready {
		t.Error("Ready = false despite the annotated container genuinely running -- container-name annotation isn't being honored")
	}
}

func TestDestroyMachine_ThenReady_ReturnsFalseAgain(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	const name = "join-test-destroy-then-ready"
	const image = "alpine:3.20"

	if err := CreateMachine(ctx, name, image); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	machine := fakeContainernetMachine(name)
	if ready, err := (Provider{}).Ready(ctx, machine); err != nil || !ready {
		t.Fatalf("precondition failed: Ready() = (%v, %v) right after CreateMachine, want (true, nil)", ready, err)
	}

	if err := DestroyMachine(ctx, name); err != nil {
		t.Fatalf("DestroyMachine: %v", err)
	}

	ready, err := Provider{}.Ready(ctx, machine)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready {
		t.Error("Ready = true after DestroyMachine -- the container should be gone")
	}
}
