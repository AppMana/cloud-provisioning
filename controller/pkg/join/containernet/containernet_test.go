package containernet

import (
	"context"
	"os/exec"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// requireDocker skips the test on any machine without a working Docker
// daemon (e.g. CI without docker-in-docker) rather than failing;
// these tests exercise a real `docker` binary rather than a mock.
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

func TestRunning_NonexistentContainer_ReturnsFalseNotError(t *testing.T) {
	requireDocker(t)
	machine := fakeContainernetMachine("join-test-definitely-does-not-exist-abc123")

	ready, err := Provider{}.Running(context.Background(), machine)
	if err != nil {
		t.Fatalf("Running on a nonexistent container must report \"not ready\", not an error: %v", err)
	}
	if ready {
		t.Error("Running = true for a container that was never created")
	}
}

func TestRunning_UsesContainerNameAnnotationWhenPresent(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	const containerName = "join-test-annotated-container-name"
	const image = "alpine:3.20"

	machine := fakeContainernetMachine("some-other-k8s-object-name")
	machine.SetAnnotations(map[string]string{containerNameAnnotation: containerName})

	// The container is made here rather than by the provider. Adopting
	// one someone else owns is the whole point of the annotation, so a
	// provider that could create it would be testing the wrong thing.
	if out, err := exec.Command("docker", "run", "-d", "--name", containerName, image, "sleep", "infinity").CombinedOutput(); err != nil {
		t.Fatalf("starting a container to adopt: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
	})

	ready, err := Provider{}.Running(ctx, machine)
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if !ready {
		t.Error("Running = false despite the annotated container genuinely running: container-name annotation isn't being honored")
	}
}
