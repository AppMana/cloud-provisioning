// Package containernet implements join.InfraProvider backed by a real
// Docker container standing in for a "machine" -- used to integration
// test the join.Reconciler entirely locally, without AWS/CAPA.
//
// This is a genuinely different shape of InfraProvider from aws.Provider:
// AWSMachine's creation is CAPA's job (a separate operator this
// reconciler only ever reads, gracefully tolerating its CRD not being
// installed yet -- see isMissingCRD in pkg/join/reconciler.go). There is
// no equivalent "containernet operator" watching a ContainernetMachine
// CRD in a real cluster, so this package's CreateMachine/DestroyMachine
// do the actual provisioning themselves, driven directly by a test/
// harness caller -- the same role CAPA plays for AWS, just invoked
// synchronously instead of via its own reconcile loop.
package containernet

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var gvk = schema.GroupVersionKind{Group: "containernet.appmana.com", Version: "v1", Kind: "ContainernetMachine"}

// containerNameAnnotation lets a ContainernetMachine reference a
// container whose name differs from the Kubernetes object's own name
// (Docker container names and Kubernetes object names don't share a
// charset) -- falls back to the object's own name when absent.
const containerNameAnnotation = "containernet.appmana.com/container-name"

// Provider implements join.InfraProvider for a Docker-container-backed
// test double.
type Provider struct{}

// GVK implements join.InfraProvider.
func (Provider) GVK() schema.GroupVersionKind { return gvk }

// Ready implements join.InfraProvider: true only if a real Docker
// container by this machine's name is actually running, checked via a
// genuine `docker inspect`, not an in-memory flag.
func (Provider) Ready(ctx context.Context, machine *unstructured.Unstructured) (bool, error) {
	name := containerName(machine)
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Running}}", name).CombinedOutput()
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)), "no such object") {
			// Not created yet (or already removed) -- "not ready", not
			// an error, mirroring aws.Provider's not-yet-ready contract.
			return false, nil
		}
		return false, fmt.Errorf("docker inspect %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	running, err := strconv.ParseBool(strings.TrimSpace(string(out)))
	if err != nil {
		return false, fmt.Errorf("parsing docker inspect output %q: %w", out, err)
	}
	return running, nil
}

// InfraValues implements join.InfraProvider. Contributes nothing extra
// today, mirroring aws.Provider's contract -- kept as a real method so
// the interface stays honest about what a provider could contribute.
func (Provider) InfraValues(ctx context.Context, machine *unstructured.Unstructured) (map[string]any, error) {
	return map[string]any{}, nil
}

// CreateMachine actually provisions the compute a ContainernetMachine
// represents: a real, running, detached Docker container. Unlike AWS
// (where CAPA does this outside the reconciler entirely), there is no
// separate operator for containernet-backed machines -- a test/harness
// caller invokes this directly, playing CAPA's role synchronously.
func CreateMachine(ctx context.Context, name, image string) error {
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name, "--network", "none", image, "sleep", "infinity").CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run %s (%s): %w: %s", name, image, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DestroyMachine tears down a container CreateMachine started, e.g.
// via a test's defer. Ignores "already gone" so cleanup after a test
// that already destroyed it (or never fully created it) isn't itself
// a spurious failure.
func DestroyMachine(ctx context.Context, name string) error {
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput()
	if err != nil && !strings.Contains(strings.ToLower(string(out)), "no such container") {
		return fmt.Errorf("docker rm -f %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func containerName(machine *unstructured.Unstructured) string {
	if n := machine.GetAnnotations()[containerNameAnnotation]; n != "" {
		return n
	}
	return machine.GetName()
}
