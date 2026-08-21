package provider

import (
	"context"
	"io"
	"io/fs"
	"strings"
	"testing"

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

// Addresses are reported on the Machine as well as the infrastructure
// machine, because Cluster API's own controllers are not running
// here: in a real cluster the machine controller copies them up, and
// with nothing doing that the consumer reads a Machine with no
// address however correct the infrastructure object is.
func TestAddressesReachTheMachineToo(t *testing.T) {
	k := &fakeKube{objects: map[string]string{
		"remote1": `{"metadata":{"name":"remote1"},"spec":{"containerName":"clab-cldt-remote1"}}`,
	}}
	c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt"}

	if _, err := c.Reconcile(context.Background(), "cloud-provisioning"); err != nil {
		t.Fatal(err)
	}
	var onMachine bool
	for _, call := range k.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "patch machine ") && strings.Contains(joined, "addresses") {
			onMachine = true
		}
	}
	if !onMachine {
		t.Error("the Machine never received the address, so the consumer reads one with none")
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
	objects map[string]string
	calls   [][]string
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
	case contains(argv, "-o") && contains(argv, "json") && !contains(argv, "jsonpath"):
		for name, body := range n.k.objects {
			if contains(argv, name) {
				return []byte(body), nil
			}
		}
		return nil, errNotFound
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
