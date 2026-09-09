// Package provider is the lab's infrastructure controller: the role
// CAPA plays for AWS.
//
// Nothing has ever played it here. The join reconciler creates a
// ContainernetMachine from a template and then waits for someone to
// say the machine exists and where it is; in AWS that someone is
// CAPA, which launches an instance and reports its addresses. In the
// lab the machine already exists, because the topology owns it, but
// something still has to observe it and report — and until now the
// harness script did that by patching the answer in, having been told
// it as an argument.
//
// That difference matters more than it sounds. A script that patches
// a known answer proves the join works when handed a correct address.
// A controller that reads the machine, resolves which node backs it,
// asks the rig what address that node actually has, and reports it,
// proves the path an operator's cluster would take. If the reconciler
// stopped creating the machine, or created it with the wrong binding,
// or the address moved, the script would keep passing and this fails.
//
// It reconciles rather than watches: the API server is reachable only
// through the bastion, so there is no informer here. What it does per
// pass is the contract, in the order the contract requires — spec
// before status, addresses before ready — so that a consumer reading
// half-applied state sees a machine that is not yet ready rather than
// one that is ready and has nowhere to be reached.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Annotation and field names, matching what the product's
// containernet provider reads.
const (
	containerNameAnnotation = "containernet.appmana.com/container-name"
	machineKind             = "containernetmachine"
	providerScheme          = "containernet://"
)

// ProviderID is how this provider names a machine. Cluster API's
// Machine controller links a Machine to a Node by matching it, so the
// infrastructure machine and the node have to agree on it exactly.
func ProviderID(node string) string { return providerScheme + node }

// Controller reports the lab's machines the way an infrastructure
// provider reports a cloud's.
type Controller struct {
	// Slots enables UID-bound allocation for generated Machine names.
	Slots    *SlotStore
	Rig      rig.Rig
	Kube     *kube.Client
	Topology lab.Topology
	// LabName prefixes container names, so that a machine bound to
	// "clab-cldt-remote1" resolves to the topology's "remote1".
	LabName string
}

// Machine is what one pass observed, for a caller that wants to
// assert on it rather than only on the cluster's state afterwards.
type Machine struct {
	ProviderID string
	Name       string
	Namespace  string
	Node       string
	Address    string
	Ready      bool
}

// Reconcile brings every machine in a namespace up to date and
// returns what it observed.
//
// Idempotent: a machine already reported is left alone, so this can
// be called in a loop without churning the API.
func (c *Controller) Reconcile(ctx context.Context, namespace string) ([]Machine, error) {
	if c.Slots != nil && c.Rig == nil {
		return nil, fmt.Errorf("pooled provisioning requires a VM rig")
	}
	if c.Slots != nil {
		if _, err := c.Slots.ReleaseStopped(ctx); err != nil {
			return nil, err
		}
	}
	names, err := c.machines(ctx, namespace)
	if err != nil {
		return nil, err
	}

	eligible, allocationErr := c.reserveEligibleBindings(ctx, namespace, names)
	if allocationErr != nil && !errors.Is(allocationErr, ErrSlotCapacity) {
		return nil, allocationErr
	}
	if err := c.checkDistinctBindings(ctx, namespace, eligible); err != nil {
		return nil, err
	}
	var out []Machine
	for _, name := range eligible {
		m, err := c.reconcileOne(ctx, namespace, name)
		if err != nil {
			return out, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, m)
	}
	return out, allocationErr
}

// ReconcileTimeout bounds one pass.
//
// Without it the loop's only limit is the caller's context, and a
// single call that does not return spends the whole of it — the
// caller then reports that a machine was never seen, when what
// happened is that nothing was ever asked a second time.
const ReconcileTimeout = 90 * time.Second

// WaitFor reconciles until every named machine is reported ready.
func (c *Controller) WaitFor(ctx context.Context, namespace string, want []string, every time.Duration) error {
	for {
		pass, cancel := context.WithTimeout(ctx, ReconcileTimeout)
		observed, err := c.Reconcile(pass, namespace)
		cancel()
		if err == nil && ready(observed, want) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %v to be reported: %w", want, ctx.Err())
		case <-time.After(every):
		}
	}
}

func ready(observed []Machine, want []string) bool {
	found := map[string]bool{}
	for _, m := range observed {
		if m.Ready {
			found[m.Name] = true
		}
	}
	for _, name := range want {
		if !found[name] {
			return false
		}
	}
	return len(want) > 0
}

func (c *Controller) machines(ctx context.Context, namespace string) ([]string, error) {
	out, err := c.Kube.Run(ctx, "-n", namespace, "get", machineKind,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// reconcileOne observes one machine and reports it.
func (c *Controller) reconcileOne(ctx context.Context, namespace, name string) (Machine, error) {
	raw, err := c.Kube.Run(ctx, "-n", namespace, "get", machineKind, name, "-o", "json")
	if err != nil {
		return Machine{}, err
	}
	var obj struct {
		Metadata struct {
			Annotations       map[string]string `json:"annotations"`
			UID               string            `json:"uid"`
			Finalizers        []string          `json:"finalizers"`
			DeletionTimestamp *string           `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec struct {
			ContainerName string `json:"containerName"`
			ProviderID    string `json:"providerID"`
		} `json:"spec"`
		Status struct {
			Ready bool `json:"ready"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Machine{}, fmt.Errorf("reading the machine: %w", err)
	}

	if c.Slots != nil && obj.Metadata.DeletionTimestamp != nil && !slices.Contains(obj.Metadata.Finalizers, instanceFinalizer) {
		return Machine{Name: name, Namespace: namespace}, nil
	}
	// Which node backs it, read from the machine rather than passed
	// in. The annotation is what the printer column shows; the spec
	// field is where a template puts it.
	binding := machineBinding(name, obj.Metadata.Annotations, obj.Spec.ContainerName)

	node, err := c.node(binding)
	if err != nil {
		return Machine{}, err
	}

	// The address is asked of the topology, not supplied by a caller.
	// It is the one fact the mesh cannot derive for itself, because it
	// is a property of the cloud the machine sits in — which is
	// exactly why a provider is the thing that reports it.
	address := node.Address(node.Interfaces[0].Segment)
	if address == "" {
		return Machine{}, fmt.Errorf("%s backs this machine but holds no address", node.Name)
	}

	id := ProviderID(node.Name)
	if c.Slots != nil {
		if obj.Metadata.UID == "" {
			return Machine{}, fmt.Errorf("pooled instance UID required")
		}
		id += "/" + obj.Metadata.UID
		if obj.Spec.ProviderID != "" && obj.Spec.ProviderID != id {
			return Machine{}, fmt.Errorf("pooled instance provider identity changed")
		}
	}
	m := Machine{ProviderID: id, Name: name, Namespace: namespace, Node: node.Name, Address: address, Ready: obj.Status.Ready}
	if c.Rig != nil {
		if obj.Metadata.DeletionTimestamp == nil {
			if err := c.setProviderID(ctx, namespace, name, id); err != nil {
				return m, err
			}
			if err := c.setAddresses(ctx, namespace, name, address); err != nil {
				return m, err
			}
		}
		booted, err := c.provision(ctx, namespace, name, node.Name, obj.Metadata.UID, obj.Metadata.Annotations, obj.Metadata.Finalizers, obj.Metadata.DeletionTimestamp != nil)
		if err != nil {
			return m, err
		}
		m.Ready = booted
		if !booted {
			return m, nil
		}
		if !obj.Status.Ready {
			if err := c.setReady(ctx, namespace, name); err != nil {
				return m, err
			}
		}
		return m, nil
	}
	if obj.Status.Ready && obj.Spec.ProviderID != "" {
		return m, nil // already reported
	}

	// The contract's own order. providerID identifies the machine to
	// everything downstream and is part of the spec, so it is set
	// before any status claims the machine is usable; addresses land
	// before ready, so that a consumer reading half-applied state sees
	// a machine that is not yet ready rather than one that is ready
	// and has nowhere to be reached.
	if err := c.setProviderID(ctx, namespace, name, id); err != nil {
		return m, err
	}
	if err := c.setAddresses(ctx, namespace, name, address); err != nil {
		return m, err
	}
	if err := c.setReady(ctx, namespace, name); err != nil {
		return m, err
	}
	m.Ready = true
	return m, nil
}

// node resolves a container name to the topology node behind it.
func (c *Controller) node(binding string) (lab.Node, error) {
	want := strings.TrimPrefix(binding, "clab-"+c.LabName+"-")
	for _, n := range c.Topology.Nodes {
		if n.Name == want {
			if n.Role != lab.Remote {
				return lab.Node{}, fmt.Errorf("%s is not a provisionable remote slot", want)
			}
			return n, nil
		}
	}
	return lab.Node{}, fmt.Errorf("no node in the topology backs %q", binding)
}

func (c *Controller) setProviderID(ctx context.Context, namespace, name, id string) error {
	patch := fmt.Sprintf(`{"spec":{"providerID":%q}}`, id)
	_, err := c.Kube.Run(ctx, "-n", namespace, "patch", machineKind, name, "--type", "merge", "-p", patch)
	return err
}

// setAddresses reports where the machine can be reached.
//
// On the infrastructure machine only. Cluster API's Machine
// controller copies them up to the Machine, and that is its job: a
// harness that wrote both would be standing in for a dependency the
// product declares, and no row would notice if that dependency were
// missing.
func (c *Controller) setAddresses(ctx context.Context, namespace, name, address string) error {
	addresses := fmt.Sprintf(
		`[{"type":"ExternalIP","address":%q},{"type":"InternalIP","address":%q}]`, address, address)

	patch := fmt.Sprintf(`{"status":{"addresses":%s}}`, addresses)
	if _, err := c.Kube.Run(ctx, "-n", namespace, "patch", machineKind, name,
		"--subresource=status", "--type", "merge", "-p", patch); err != nil {
		return fmt.Errorf("reporting %s's address: %w", name, err)
	}
	return nil
}

// setReady says the machine is usable, in the terms the contract in
// use requires.
//
// Both fields. status.ready is the v1beta1 contract; the v1beta2
// contract replaced it with status.initialization.provisioned, and
// Cluster API v1.11 reads only the latter — it reported
// "ContainernetMachine status.initialization.provisioned is false"
// while status.ready had been true the whole time. The CRD declares
// both contracts, so it satisfies both.
func (c *Controller) setReady(ctx context.Context, namespace, name string) error {
	_, err := c.Kube.Run(ctx, "-n", namespace, "patch", machineKind, name,
		"--subresource=status", "--type", "merge",
		"-p", `{"status":{"ready":true,"initialization":{"provisioned":true}}}`)
	return err
}

// SetNodeProviderID tells Kubernetes which machine a node is.
//
// In a cloud this is the cloud controller manager's job, or kubelet's
// when it is started with a provider. Here the lab is the cloud, so
// the lab does it — and this is the line worth keeping straight: it
// is the CLOUD's job, not Cluster API's. Cluster API's Machine
// controller then links the Machine to the Node by matching this
// against the Machine's own providerID, and this harness does not
// write nodeRef at all.
//
// A node whose providerID is already set is left alone: the field is
// immutable once written.
func (c *Controller) SetNodeProviderID(ctx context.Context, node, providerID string) error {
	existing, err := c.Kube.Get(ctx, "", "node", node, "{.spec.providerID}")
	if err != nil {
		return err
	}
	if existing != "" {
		return nil
	}
	patch := fmt.Sprintf(`{"spec":{"providerID":%q}}`, providerID)
	if _, err := c.Kube.Run(ctx, "patch", "node", node, "--type", "merge", "-p", patch); err != nil {
		return fmt.Errorf("giving node %s its provider identity: %w", node, err)
	}
	return nil
}

// AdoptNodes gives every node that a machine reported an address for
// the provider identity Cluster API matches on.
//
// Found by address rather than by name, because the address is the
// fact the provider already reported: a machine bound to the wrong
// node then adopts nothing instead of adopting the wrong node.
func (c *Controller) AdoptNodes(ctx context.Context, namespace string, every time.Duration) error {
	for {
		pass, cancel := context.WithTimeout(ctx, ReconcileTimeout)
		machines, err := c.Reconcile(pass, namespace)
		cancel()
		if err == nil {
			adopted := len(machines) > 0
			for _, m := range machines {
				node, err := c.nodeAt(ctx, m.Address)
				if err != nil {
					return err
				}
				if node == "" {
					if c.Rig != nil {
						if health, ok := c.Rig.Node(m.Node).(rig.BootstrapHealth); ok {
							if err := health.BootstrapFailure(ctx); err != nil {
								return err
							}
						}
					}
					adopted = false
					continue
				}
				if err := c.SetNodeProviderID(ctx, node, m.ProviderID); err != nil {
					return err
				}
			}
			if adopted {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no node took the provider's identity: %w", ctx.Err())
		case <-time.After(every):
		}
	}
}

// nodeAt finds the node holding an address.
func (c *Controller) nodeAt(ctx context.Context, address string) (string, error) {
	out, err := c.Kube.Run(ctx, "get", "nodes", "-o",
		`jsonpath={range .items[*]}{.metadata.name}{" "}{range .status.addresses[*]}{.address}{","}{end}{"\n"}{end}`)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		name, addrs, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		for _, a := range strings.Split(addrs, ",") {
			if a == address {
				return name, nil
			}
		}
	}
	return "", nil
}

// ReconcileCluster reports the infrastructure cluster.
//
// Cluster API's Cluster controller will not move a Cluster out of
// Provisioning until its infrastructure reports ready, and until the
// Cluster is provisioned every Machine in it stays Pending — so
// nothing is ever linked to a node and the mesh is never told a peer
// exists. That is a real provider responsibility, not a lab detail:
// CAPA reports an AWSCluster the same way, once the load balancer and
// the network exist.
//
// The endpoint is stated because Cluster API requires one before it
// considers a cluster usable. It names a real control plane: the site
// has no address that moves between nodes, so any member is as good
// as any other and the first is chosen for being deterministic.
func (c *Controller) ReconcileCluster(ctx context.Context, namespace, name, host string, port int) error {
	ready, err := c.Kube.Get(ctx, namespace, "containernetcluster", name, "{.status.ready}")
	if err != nil {
		return fmt.Errorf("reading the infrastructure cluster: %w", err)
	}
	// Spec before status, as everywhere else: an endpoint has to be
	// readable before anything is told the cluster is usable.
	endpoint := fmt.Sprintf(`{"spec":{"controlPlaneEndpoint":{"host":%q,"port":%d}}}`, host, port)
	if _, err := c.Kube.Run(ctx, "-n", namespace, "patch", "containernetcluster", name,
		"--type", "merge", "-p", endpoint); err != nil {
		return fmt.Errorf("stating the cluster's endpoint: %w", err)
	}
	// A retained cluster can be Ready while advertising an obsolete endpoint.
	if ready == "true" {
		return nil
	}
	if _, err := c.Kube.Run(ctx, "-n", namespace, "patch", "containernetcluster", name,
		"--subresource=status", "--type", "merge",
		"-p", `{"status":{"ready":true,"initialization":{"provisioned":true}}}`); err != nil {
		return fmt.Errorf("reporting the cluster ready: %w", err)
	}
	return nil
}

func (c *Controller) patchMetadata(ctx context.Context, namespace, name string, metadata map[string]any) error {
	patch, err := json.Marshal(map[string]any{"metadata": metadata})
	if err != nil {
		return err
	}
	_, err = c.Kube.Run(ctx, "-n", namespace, "patch", machineKind, name, "--type", "merge", "-p", string(patch))
	return err
}
