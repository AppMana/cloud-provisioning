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

// ObservesAddresses implements join.AddressObserver: this provider
// adopts machines that already exist, so where one is is known before
// it is bootstrapped, and there is no reason to render a document
// that cannot say.
func (Provider) ObservesAddresses() bool { return true }

// InfraValues implements join.InfraProvider: it tells the machine
// which of its addresses is the one this provider reports.
//
// A node left to choose gets it wrong wherever there is more than one
// address to choose from. On a machine with an out-of-band interface
// beside its real one the kubelet registered the out-of-band address
// — the same on every machine, and part of no network the cluster
// models — while the mesh had already published the other one. The
// node joined, went Ready, and carried an identity nothing was
// looking for, so it was never adopted.
//
// This is not a guess: it is the address this provider reports to
// Cluster API and the address the peer list is built from, so a
// kubelet pinned to it agrees with everything that already believes
// it. Where none has been reported yet, nothing is said, and the
// distribution works it out however it normally would.
func (Provider) InfraValues(ctx context.Context, machine *unstructured.Unstructured) (map[string]any, error) {
	values := map[string]any{}
	if addr := reportedAddress(machine); addr != "" {
		values["nodeAddress"] = addr
	}
	// And the identity Cluster API binds a Machine to a Node by.
	//
	// Some distributions assign one themselves — k3s gives every node
	// k3s://<name> as it registers, RKE2 the same — so a node that is
	// not told otherwise carries an identity the Machine does not
	// have. nodeRef is then never set, the controller that publishes
	// the remote's pod block never finds a node, and the remote joins,
	// goes Ready and is unreachable from the site with everything
	// reporting healthy.
	if id, found, err := unstructured.NestedString(machine.Object, "spec", "providerID"); err == nil && found && id != "" {
		values["providerID"] = id
	}
	return values, nil
}

// reportedAddress is the address this provider published for the
// machine. InternalIP first: that is the address a node is reached by
// from inside the cluster, which is what a kubelet is registering.
func reportedAddress(machine *unstructured.Unstructured) string {
	addresses, found, err := unstructured.NestedSlice(machine.Object, "status", "addresses")
	if err != nil || !found {
		return ""
	}
	var fallback string
	for _, entry := range addresses {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		address, _ := item["address"].(string)
		if address == "" {
			continue
		}
		if item["type"] == "InternalIP" {
			return address
		}
		if fallback == "" {
			fallback = address
		}
	}
	return fallback
}

// Nothing here creates or destroys a container.
//
// A machine is adopted, not made: containerlab, or whatever else owns
// the topology, creates it and this observes it. The functions that used
// to live here ran "docker run --network none ... sleep infinity", which
// attaches a node to no segment at all, and were called by nothing but
// their own test.
// containerName is which container backs this machine, freshest
// source first.
//
// The annotation wins because it is what the printer column shows and
// what already-created objects carry. Then the machine's own spec,
// which is where a template puts it: a ContainernetMachineTemplate
// carries spec.template.spec.containerName and the claim reconciler
// copies that spec into the machine it creates, the same way an
// AWSMachineTemplate's instanceType reaches an AWSMachine. Reading
// only the annotation left that field dead, so the binding had to be
// written twice — once where the API says it goes and once where the
// code actually looked.
//
// Failing that, the object's own name: a machine and a container may
// share one, and guessing is better than returning nothing.
func containerName(machine *unstructured.Unstructured) string {
	if n := machine.GetAnnotations()[containerNameAnnotation]; n != "" {
		return n
	}
	if n, found, err := unstructured.NestedString(machine.Object, "spec", "containerName"); err == nil && found && n != "" {
		return n
	}
	return machine.GetName()
}
