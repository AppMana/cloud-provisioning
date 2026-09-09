package provider

import (
	"context"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// The address is derived from the machine, not handed to the
// controller.
//
// This is the whole difference between a provider and the script it
// replaces. The script was told the address as an argument, so it
// proved the join works when given a correct one; the controller
// reads which node backs the machine and asks the topology where that
// node is, so a wrong binding or a moved address fails here instead
// of passing.
func TestTheAddressIsDerivedFromTheMachine(t *testing.T) {
	k := &fakeKube{objects: map[string]string{
		"remote1": `{"metadata":{"name":"remote1"},"spec":{"containerName":"clab-cldt-remote1"}}`,
	}}
	c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt"}

	got, err := c.Reconcile(context.Background(), "cloud-provisioning")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("observed %d machines, want 1", len(got))
	}
	if got[0].Node != "remote1" {
		t.Errorf("resolved node %q", got[0].Node)
	}
	// Cloud A's address, from the topology — never supplied here.
	if got[0].Address != lab.CloudAPrefix+".10" {
		t.Errorf("address %q, want the one the topology gives remote1", got[0].Address)
	}
	if !got[0].Ready {
		t.Error("the machine was not reported ready")
	}
}

// Preserve the lab binding contract at its current owner. Older product-side
// Docker inspection helpers tested these cases without provisioning a node.
func TestMachineBindingPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, machine, body, node, address string
	}{
		{"spec", "public-worker", `{"metadata":{"name":"public-worker"},"spec":{"containerName":"clab-cldt-remote1"}}`, "remote1", lab.CloudAPrefix + ".10"},
		{"annotation overrides spec", "public-worker", `{"metadata":{"name":"public-worker","annotations":{"containernet.appmana.com/container-name":"clab-cldt-remote2"}},"spec":{"containerName":"clab-cldt-remote1"}}`, "remote2", lab.CloudBPrefix + ".10"},
		{"object name fallback", "remote1", `{"metadata":{"name":"remote1"}}`, "remote1", lab.CloudAPrefix + ".10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &fakeKube{objects: map[string]string{tc.machine: tc.body}}
			c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt"}
			got, err := c.Reconcile(context.Background(), "cloud-provisioning")
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("observed %d machines, want 1", len(got))
			}
			if got[0].Node != tc.node || got[0].Address != tc.address || !got[0].Ready {
				t.Fatalf("binding resolved to %+v, want ready %s at %s", got[0], tc.node, tc.address)
			}
		})
	}
}

// A machine bound to something the topology does not have is an
// error, not a machine reported at some default address. Reporting a
// wrong address would strand the join somewhere far from the cause.
func TestAnUnknownBindingIsAnError(t *testing.T) {
	k := &fakeKube{objects: map[string]string{
		"stray": `{"metadata":{"name":"stray"},"spec":{"containerName":"clab-cldt-nowhere"}}`,
	}}
	c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt"}

	_, err := c.Reconcile(context.Background(), "cloud-provisioning")
	if err == nil {
		t.Fatal("a machine bound to a node that does not exist was reported anyway")
	}
	if !strings.Contains(err.Error(), "nowhere") {
		t.Errorf("the error does not name the binding it could not resolve: %v", err)
	}
}

// The contract's order: providerID before any status, addresses
// before ready. A consumer reading half-applied state must see a
// machine that is not yet ready rather than one that is ready and has
// nowhere to be reached.
func TestTheContractIsAppliedInOrder(t *testing.T) {
	k := &fakeKube{objects: map[string]string{
		"remote1": `{"metadata":{"name":"remote1"},"spec":{"containerName":"clab-cldt-remote1"}}`,
	}}
	c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt"}

	if _, err := c.Reconcile(context.Background(), "cloud-provisioning"); err != nil {
		t.Fatal(err)
	}

	providerID, addresses, ready := -1, -1, -1
	for i, call := range k.calls {
		joined := strings.Join(call, " ")
		switch {
		case strings.Contains(joined, "providerID"):
			providerID = i
		case strings.Contains(joined, "addresses"):
			if addresses < 0 {
				addresses = i
			}
		case strings.Contains(joined, `"ready":true`):
			ready = i
		}
	}
	if providerID < 0 || addresses < 0 || ready < 0 {
		t.Fatalf("the contract was not applied: providerID=%d addresses=%d ready=%d", providerID, addresses, ready)
	}
	if !(providerID < addresses && addresses < ready) {
		t.Errorf("out of order: providerID=%d addresses=%d ready=%d", providerID, addresses, ready)
	}
}

// Addresses go on the infrastructure machine and nowhere else.
//
// Copying them up to the Machine is Cluster API's Machine controller's
// job. A harness that wrote both would be standing in for a
// dependency the product declares, and no row would notice if that
// dependency were missing — which is exactly what happened for as
// long as the lab ran without Cluster API installed.
func TestAddressesGoOnTheInfrastructureMachineOnly(t *testing.T) {
	k := &fakeKube{objects: map[string]string{
		"remote1": `{"metadata":{"name":"remote1"},"spec":{"containerName":"clab-cldt-remote1"}}`,
	}}
	c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt"}

	if _, err := c.Reconcile(context.Background(), "cloud-provisioning"); err != nil {
		t.Fatal(err)
	}
	var onInfra bool
	for _, call := range k.calls {
		joined := strings.Join(call, " ")
		if !strings.Contains(joined, "addresses") {
			continue
		}
		switch {
		case strings.Contains(joined, "patch containernetmachine"):
			onInfra = true
		case strings.Contains(joined, "patch machine "):
			t.Errorf("the harness wrote the Machine's addresses, which Cluster API copies up: %v", call)
		}
	}
	if !onInfra {
		t.Error("the infrastructure machine never received its address")
	}
}

// A machine already reported is left alone, so the loop does not
// churn the API.
func TestAReportedMachineIsNotRewritten(t *testing.T) {
	k := &fakeKube{objects: map[string]string{
		"remote1": `{"metadata":{"name":"remote1"},"spec":{"containerName":"clab-cldt-remote1",` +
			`"providerID":"containernet://remote1"},"status":{"ready":true}}`,
	}}
	c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt"}

	if _, err := c.Reconcile(context.Background(), "cloud-provisioning"); err != nil {
		t.Fatal(err)
	}
	for _, call := range k.calls {
		if strings.Contains(strings.Join(call, " "), "patch") {
			t.Errorf("an already-reported machine was patched again: %v", call)
		}
	}
}

// fakeKube answers kubectl through a bastion that is not there.
type fakeKube struct {
	clusterReady string
	objects      map[string]string
	nodes        string
	calls        [][]string
}

func (f *fakeKube) client() *kube.Client {
	return &kube.Client{Bastion: &fakeNode{k: f}, ControlPlanes: []string{"10.10.0.10"}}
}

type fakeNode struct{ k *fakeKube }

func (n *fakeNode) Name() string { return "bastion" }

func (n *fakeNode) Exec(ctx context.Context, argv ...string) ([]byte, error) {
	// The readiness probe the client makes before every call.
	if contains(argv, "/readyz") {
		return nil, nil
	}
	// Strip the kubectl and --server words so assertions read as the
	// command the caller meant.
	var call []string
	for _, a := range argv {
		if a == "kubectl" || strings.HasPrefix(a, "--server=") {
			continue
		}
		call = append(call, a)
	}
	n.k.calls = append(n.k.calls, call)

	switch {
	case containsPrefix(argv, "jsonpath={.status.ready}"):
		return []byte(n.k.clusterReady), nil
	case contains(argv, "-o") && contains(argv, "json") && !contains(argv, "jsonpath"):
		for name, body := range n.k.objects {
			if contains(argv, name) {
				return []byte(body), nil
			}
		}
		return nil, errNotFound
	case containsPrefix(argv, "jsonpath=") && contains(argv, "nodes"):
		return []byte(n.k.nodes), nil
	case containsPrefix(argv, "jsonpath={.spec.providerID}"):
		return nil, nil
	case containsPrefix(argv, "jsonpath={.status.nodeRef.name}"):
		// Unlinked until something links it, which is the state every
		// machine starts in.
		return nil, nil
	case containsPrefix(argv, "jsonpath="):
		var names []string
		for name := range n.k.objects {
			names = append(names, name)
		}
		return []byte(strings.Join(names, "\n")), nil
	default:
		return nil, nil
	}
}

func (n *fakeNode) Pipe(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	return n.Exec(ctx, argv...)
}
func (n *fakeNode) Put(ctx context.Context, src io.Reader, dst string, mode fs.FileMode) error {
	return nil
}
func (n *fakeNode) Cut(ctx context.Context) error                          { return nil }
func (n *fakeNode) Restore(ctx context.Context) error                      { return nil }
func (n *fakeNode) Kill(ctx context.Context) error                         { return nil }
func (n *fakeNode) Boot(ctx context.Context) error                         { return nil }
func (n *fakeNode) Userdata(ctx context.Context, cloudConfig []byte) error { return nil }

func contains(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func containsPrefix(argv []string, prefix string) bool {
	for _, a := range argv {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

var errNotFound = &notFound{}

type notFound struct{}

func (*notFound) Error() string { return "not found" }

// The cloud tells Kubernetes which machine a node is, and Cluster
// API links the Machine to it from there.
//
// That split is the point. Setting spec.providerID on the Node is the
// cloud's job — a cloud controller manager does it, or kubelet with a
// provider — and here the lab is the cloud. Writing nodeRef is
// Cluster API's job, and this harness does not do it at all: if it
// did, no row would notice a dependency the product declares being
// absent.
func TestTheCloudGivesANodeItsIdentityAndNothingMore(t *testing.T) {
	k := &fakeKube{
		objects: map[string]string{
			"remote1": `{"metadata":{"name":"remote1"},"spec":{"containerName":"clab-cldt-remote1"}}`,
		},
		nodes: "cp 10.10.0.10,\nremote1 203.0.113.10,\n",
	}
	c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt"}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.AdoptNodes(ctx, "cloud-provisioning", 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	var gaveIdentity bool
	for _, call := range k.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "patch node") && strings.Contains(joined, ProviderID("remote1")) {
			gaveIdentity = true
		}
		if strings.Contains(joined, "nodeRef") {
			t.Errorf("the harness wrote nodeRef, which is Cluster API's job: %v", call)
		}
	}
	if !gaveIdentity {
		t.Errorf("the node was never given the provider's identity: %v", k.calls)
	}
}

// The node is found by the address the provider reported, not by
// name. A machine bound to the wrong node would otherwise adopt that
// node and fail somewhere far from the cause.
func TestTheNodeIsFoundByAddressNotByName(t *testing.T) {
	k := &fakeKube{
		objects: map[string]string{
			"remote1": `{"metadata":{"name":"remote1"},"spec":{"containerName":"clab-cldt-remote1"}}`,
		},
		// A node named remote1 exists, but at the wrong address.
		nodes: "remote1 10.10.0.99,\n",
	}
	c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt"}

	node, err := c.nodeAt(context.Background(), lab.CloudAPrefix+".10")
	if err != nil {
		t.Fatal(err)
	}
	if node != "" {
		t.Errorf("matched node %q on name while its address differs", node)
	}
}

// An address the provider reported and a node that holds it is a
// match, whatever either is called.
func TestANodeHoldingTheAddressIsTheMachine(t *testing.T) {
	k := &fakeKube{nodes: "some-other-name 203.0.113.10,\n"}
	c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt"}

	node, err := c.nodeAt(context.Background(), lab.CloudAPrefix+".10")
	if err != nil {
		t.Fatal(err)
	}
	if node != "some-other-name" {
		t.Errorf("nodeAt = %q, want the node holding the address", node)
	}
}

// Interface is what this node calls the lab's nth link. A fake stands
// in for a container, which calls it what the topology does.
func (n *fakeNode) Interface(nth int) string { return "eth" + strconv.Itoa(nth+1) }
