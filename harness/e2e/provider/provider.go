// Package provider is the lab's infrastructure controller: the role
// CAPA plays for AWS.
//
// Nothing has ever played it here. The join reconciler creates a
// LabMachine from a template and then waits for someone to
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
	labcapi "github.com/appmana/labcontainers/pkg/capi"
)

// Annotation and field names, matching what the product's
// Labcontainers provider reads.
const (
	nodeNameAnnotation = "infrastructure.labcontainers.appmana.com/node-name"
	machineKind        = "labmachine"
)

// ProviderID is how this provider names a machine. Cluster API's
// Machine controller links a Machine to a Node by matching it, so the
// infrastructure machine and the node have to agree on it exactly.
func ProviderID(labName, node string) string { return labcapi.ProviderID(labName, node, "") }

// Controller reports the lab's machines the way an infrastructure
// provider reports a cloud's.
type Controller struct {
	// Slots enables UID-bound allocation for generated Machine names.
	Slots    *SlotStore
	Rig      rig.Rig
	Kube     *kube.Client
	Topology lab.Topology
	// LabName prefixes container names, so that a machine bound to
	// "remote1" resolves to the topology's "remote1".
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
			NodeName   string `json:"nodeName"`
			ProviderID string `json:"providerID"`
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
	binding := machineBinding(name, obj.Metadata.Annotations, obj.Spec.NodeName)

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

	id := ProviderID(c.LabName, node.Name)
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
	want := binding
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
// "LabMachine status.initialization.provisioned is false"
// while status.ready had been true the whole time. The CRD declares
// both contracts, so it satisfies both.
func (c *Controller) setReady(ctx context.Context, namespace, name string) error {
	_, err := c.Kube.Run(ctx, "-n", namespace, "patch", machineKind, name,
		"--subresource=status", "--type", "merge",
		"-p", `{"status":{"ready":true,"initialization":{"provisioned":true}}}`)
	return err
}

// WaitRegistration waits for every reported machine's node to register,
// and checks that it registered under the machine's provider identity.
//
// The identity is the product's to set: its join pattern starts the
// kubelet with --provider-id, and Cluster API links the Machine to the
// Node by matching it. The harness never writes a Node, so a node that
// registered without it, or under another machine's, is the product's
// failure. The field is immutable, so that failure is final.
//
// Found by address rather than by name, because the address is the
// fact the provider already reported: a machine bound to the wrong
// node then matches nothing instead of the wrong node.
func (c *Controller) WaitRegistration(ctx context.Context, namespace string, every time.Duration) error {
	for {
		pass, cancel := context.WithTimeout(ctx, ReconcileTimeout)
		machines, err := c.Reconcile(pass, namespace)
		cancel()
		if err == nil {
			registered := len(machines) > 0
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
					registered = false
					continue
				}
				got, err := c.Kube.Get(ctx, "", "node", node, "{.spec.providerID}")
				if err != nil {
					return err
				}
				if got != m.ProviderID {
					return fmt.Errorf("node %s registered with providerID %q, want %q from the product's join", node, got, m.ProviderID)
				}
			}
			if registered {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no node registered for the reported machines: %w", ctx.Err())
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

// ReconcileCluster states the site's API endpoint on its LabCluster, which an
// operator does for an existing cluster. The LabCluster is annotated
// managed-by external, so reporting it ready is the product's job.
func (c *Controller) ReconcileCluster(ctx context.Context, namespace, name, host string, port int) error {
	endpoint := fmt.Sprintf(`{"spec":{"controlPlaneEndpoint":{"host":%q,"port":%d}}}`, host, port)
	if _, err := c.Kube.Run(ctx, "-n", namespace, "patch", "labcluster", name,
		"--type", "merge", "-p", endpoint); err != nil {
		return fmt.Errorf("stating the cluster's endpoint: %w", err)
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
