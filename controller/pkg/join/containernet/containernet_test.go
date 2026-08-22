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

// The provider reports where a machine can be reached, so it can also
// tell the machine's own kubelet which address that is.
//
// A node that picks its own address gets it wrong wherever there is
// more than one to pick from. Measured on a machine with a management
// interface beside its real one: the kubelet registered 10.0.0.15,
// the out-of-band address every machine in the lab shares, instead of
// the 203.0.113.10 the mesh had already published for it. It joined,
// went Ready, and then no node carried the identity the provider was
// looking for, so nothing was ever adopted.
//
// The address is not a guess here. It is the same one this provider
// reports to Cluster API and the same one the peer list is built
// from, so a kubelet pinned to it agrees with everything else that
// already believes it.
func TestInfraValuesTellTheNodeItsOwnAddress(t *testing.T) {
	machine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "ContainernetMachine",
		"metadata":   map[string]any{"name": "remote1", "namespace": "default"},
		"status": map[string]any{
			"addresses": []any{
				map[string]any{"type": "ExternalIP", "address": "203.0.113.10"},
				map[string]any{"type": "InternalIP", "address": "203.0.113.10"},
			},
		},
	}}

	values, err := Provider{}.InfraValues(context.Background(), machine)
	if err != nil {
		t.Fatal(err)
	}
	if values["nodeAddress"] != "203.0.113.10" {
		t.Errorf("the provider reports 203.0.113.10 and tells the node %q, "+
			"so the kubelet is left to pick from every interface it has", values["nodeAddress"])
	}
}

// With no address reported there is nothing to say, and saying
// nothing has to leave the node exactly as it was: free to work it
// out however its distribution does.
func TestInfraValuesSayNothingWhenNoAddressIsReported(t *testing.T) {
	machine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "ContainernetMachine",
		"metadata":   map[string]any{"name": "remote1", "namespace": "default"},
	}}

	values, err := Provider{}.InfraValues(context.Background(), machine)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := values["nodeAddress"]; ok && got != "" {
		t.Errorf("an address was invented from nothing: %q", got)
	}
}

// The provider's identity for a machine reaches the node, because
// Cluster API binds the two by it.
//
// Some distributions assign one themselves. k3s gives every node
// k3s://<name> as it registers, and RKE2 does the same, so a node
// that is not told otherwise ends up carrying an identity the Machine
// does not have: spec.providerID says containernet://remote1, the
// node says k3s://remote1, status.nodeRef is never set, and the
// controller that publishes the remote's pod block never finds a node
// to publish for. The remote joins, goes Ready, and nothing at the
// site can reach a pod on it, with every component reporting healthy.
func TestInfraValuesCarryTheProviderIdentity(t *testing.T) {
	machine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "ContainernetMachine",
		"metadata":   map[string]any{"name": "remote1", "namespace": "default"},
		"spec":       map[string]any{"providerID": "containernet://remote1"},
	}}

	values, err := Provider{}.InfraValues(context.Background(), machine)
	if err != nil {
		t.Fatal(err)
	}
	if values["providerID"] != "containernet://remote1" {
		t.Errorf("the node is told %q, so a distribution that names itself wins and "+
			"Cluster API never binds the Machine to it", values["providerID"])
	}
}

// With none assigned yet there is nothing to say, and the node keeps
// whatever its distribution or cloud provider gives it.
func TestInfraValuesSayNothingWithoutAProviderIdentity(t *testing.T) {
	machine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "ContainernetMachine",
		"metadata":   map[string]any{"name": "remote1", "namespace": "default"},
	}}
	values, err := Provider{}.InfraValues(context.Background(), machine)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := values["providerID"]; ok && got != "" {
		t.Errorf("an identity was invented from nothing: %q", got)
	}
}
