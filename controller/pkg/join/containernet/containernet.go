// Package containernet implements join.InfraProvider backed by a real
// Docker container standing in for a "machine", used to integration
// test the join.Reconciler locally, without AWS/CAPA.
//
// This is a different shape of InfraProvider from aws.Provider:
// AWSMachine's creation is CAPA's job (a separate operator this
// reconciler only reads, tolerating its CRD not being installed yet;
// see isMissingCRD in pkg/join/reconciler.go). There is no equivalent
// containernet operator watching a ContainernetMachine CRD in a real
// cluster, so this package's CreateMachine/DestroyMachine do the
// provisioning themselves, driven by a test or harness caller: the
// same role CAPA plays for AWS, invoked synchronously instead of via
// its own reconcile loop.
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

// v1beta2 rather than v1: the claim reconciler creates an infra machine
// from a template at a hardcoded v1beta2 and then reads it back at the
// provider's own version, so a provider naming any other version would
// write an object it cannot then find.
var gvk = schema.GroupVersionKind{Group: "containernet.appmana.com", Version: "v1beta2", Kind: "ContainernetMachine"}

// clusterGVK is what a claim's CAPI Cluster points at for this provider
// to be the one that fulfils it.
var clusterGVK = schema.GroupVersionKind{Group: "containernet.appmana.com", Version: "v1beta2", Kind: "ContainernetCluster"}

// containerNameAnnotation lets a ContainernetMachine reference a
// container whose name differs from the Kubernetes object's own name
// (Docker container names and Kubernetes object names don't share a
// charset). It falls back to the object's own name when absent.
const containerNameAnnotation = "containernet.appmana.com/container-name"

// Provider implements join.InfraProvider for a Docker-container-backed
// test double.
type Provider struct{}

// GVK implements join.InfraProvider.
func (Provider) GVK() schema.GroupVersionKind { return gvk }

// ClusterGVK implements join.MachineProvisioner. Without it this is not
// a provisioner at all and the claim reconciler refuses every claim
// routed to it, which is the only reason a claim can name it.
func (Provider) ClusterGVK() schema.GroupVersionKind { return clusterGVK }

// Running reports whether a real Docker container by this machine's
// name is running, checked via `docker inspect` rather than an
// in-memory flag. This is a test helper; the reconciler does not gate
// on infrastructure readiness, because userdata has to exist before
// the compute launches (see pkg/join/reconciler.go).
func (Provider) Running(ctx context.Context, machine *unstructured.Unstructured) (bool, error) {
	name := containerName(machine)
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Running}}", name).CombinedOutput()
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)), "no such object") {
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

// InfraValues implements join.InfraProvider. It contributes nothing
// extra today, mirroring aws.Provider's contract, and is kept as a
// real method so the interface still shows what a provider can
// contribute.
func (Provider) InfraValues(ctx context.Context, machine *unstructured.Unstructured) (map[string]any, error) {
	return map[string]any{}, nil
}

// Nothing here creates or destroys a container.
//
// A machine is adopted, not made: containerlab, or whatever else owns
// the topology, creates it and this observes it. The functions that used
// to live here ran "docker run --network none ... sleep infinity", which
// attaches a node to no segment at all, and were called by nothing but
// their own test.
func containerName(machine *unstructured.Unstructured) string {
	if n := machine.GetAnnotations()[containerNameAnnotation]; n != "" {
		return n
	}
	return machine.GetName()
}
