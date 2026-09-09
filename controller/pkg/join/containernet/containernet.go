// Package containernet supplies infrastructure values for the lab's CAPI
// contract. The harness/e2e/provider controller owns compute provisioning
// through its VM or legacy container rig; this package reads the resulting
// address and provider identity for product bootstrap templates.
package containernet

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// gvk matches the pinned lab infrastructure contract installed by the harness.
var gvk = schema.GroupVersionKind{Group: "containernet.appmana.com", Version: "v1beta2", Kind: "ContainernetMachine"}

// clusterGVK is what a claim's CAPI Cluster points at for this provider
// to be the one that fulfils it.
var clusterGVK = schema.GroupVersionKind{Group: "containernet.appmana.com", Version: "v1beta2", Kind: "ContainernetCluster"}

// Provider implements join.InfraProvider for lab machines.
type Provider struct{}

// GVK implements join.InfraProvider.
func (Provider) GVK() schema.GroupVersionKind { return gvk }

// ClusterGVK implements join.MachineProvisioner. Without it this is not
// a provisioner at all and the claim reconciler refuses every claim
// routed to it, which is the only reason a claim can name it.
func (Provider) ClusterGVK() schema.GroupVersionKind { return clusterGVK }

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
