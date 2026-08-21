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
	"fmt"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// Annotation and field names, matching what the product's
// containernet provider reads.
const (
	containerNameAnnotation = "containernet.appmana.com/container-name"
	machineKind             = "containernetmachine"
)

// Controller reports the lab's machines the way an infrastructure
// provider reports a cloud's.
type Controller struct {
	Kube     *kube.Client
	Topology lab.Topology
	// LabName prefixes container names, so that a machine bound to
	// "clab-cldt-remote1" resolves to the topology's "remote1".
	LabName string
}

// Machine is what one pass observed, for a caller that wants to
// assert on it rather than only on the cluster's state afterwards.
type Machine struct {
	Name      string
	Namespace string
	Node      string
	Address   string
	Ready     bool
}

// Reconcile brings every machine in a namespace up to date and
// returns what it observed.
//
// Idempotent: a machine already reported is left alone, so this can
// be called in a loop without churning the API.
func (c *Controller) Reconcile(ctx context.Context, namespace string) ([]Machine, error) {
	names, err := c.machines(ctx, namespace)
	if err != nil {
		return nil, err
	}

	var out []Machine
	for _, name := range names {
		m, err := c.reconcileOne(ctx, namespace, name)
		if err != nil {
			return out, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// WaitFor reconciles until every named machine is reported ready.
func (c *Controller) WaitFor(ctx context.Context, namespace string, want []string, every time.Duration) error {
	for {
		observed, err := c.Reconcile(ctx, namespace)
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
			Annotations map[string]string `json:"annotations"`
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

	// Which node backs it, read from the machine rather than passed
	// in. The annotation is what the printer column shows; the spec
	// field is where a template puts it.
	binding := obj.Metadata.Annotations[containerNameAnnotation]
	if binding == "" {
		binding = obj.Spec.ContainerName
	}
	if binding == "" {
		binding = name
	}

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

	m := Machine{Name: name, Namespace: namespace, Node: node.Name, Address: address, Ready: obj.Status.Ready}
	if obj.Status.Ready && obj.Spec.ProviderID != "" {
		return m, nil // already reported
	}

	// The contract's own order. providerID identifies the machine to
	// everything downstream and is part of the spec, so it is set
	// before any status claims the machine is usable; addresses land
	// before ready, so that a consumer reading half-applied state sees
	// a machine that is not yet ready rather than one that is ready
	// and has nowhere to be reached.
	if err := c.setProviderID(ctx, namespace, name, node.Name); err != nil {
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
			return n, nil
		}
	}
	return lab.Node{}, fmt.Errorf("no node in the topology backs %q", binding)
}

func (c *Controller) setProviderID(ctx context.Context, namespace, name, node string) error {
	patch := fmt.Sprintf(`{"spec":{"providerID":%q}}`, "containernet://"+node)
	_, err := c.Kube.Run(ctx, "-n", namespace, "patch", machineKind, name, "--type", "merge", "-p", patch)
	return err
}

// setAddresses reports where the machine can be reached, on the
// infrastructure machine and on the Machine that owns it.
//
// Both, because Cluster API's own controllers are not running here:
// in a real cluster the machine controller copies an infrastructure
// machine's addresses up, and with nothing doing that the consumer
// would read a Machine with no address however correct the
// infrastructure object was.
func (c *Controller) setAddresses(ctx context.Context, namespace, name, address string) error {
	addresses := fmt.Sprintf(
		`[{"type":"ExternalIP","address":%q},{"type":"InternalIP","address":%q}]`, address, address)

	for _, kind := range []string{machineKind, "machine"} {
		patch := fmt.Sprintf(`{"status":{"addresses":%s}}`, addresses)
		if _, err := c.Kube.Run(ctx, "-n", namespace, "patch", kind, name,
			"--subresource=status", "--type", "merge", "-p", patch); err != nil {
			return fmt.Errorf("reporting %s's address on the %s: %w", name, kind, err)
		}
	}
	return nil
}

func (c *Controller) setReady(ctx context.Context, namespace, name string) error {
	_, err := c.Kube.Run(ctx, "-n", namespace, "patch", machineKind, name,
		"--subresource=status", "--type", "merge", "-p", `{"status":{"ready":true}}`)
	return err
}
