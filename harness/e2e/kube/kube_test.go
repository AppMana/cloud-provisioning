package kube

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// A member that cannot answer is skipped and the next is tried. This
// is what lets an outage row keep observing while the victim is down.
func TestADeadMemberIsSkipped(t *testing.T) {
	b := &fakeNode{
		fail: func(argv []string) bool {
			return strings.Contains(strings.Join(argv, " "), "10.10.0.10")
		},
	}
	c := &Client{Bastion: b, ControlPlanes: []string{"10.10.0.10", "10.10.0.13", "10.10.0.14"}}

	server, err := c.Server(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if server != "https://10.10.0.13:6443" {
		t.Errorf("picked %s, want the first member that answered", server)
	}
}

// MicroK8s serves its API on 16443. Failover must keep that port when moving
// to another member; falling back to 6443 makes a healthy site unreachable.
func TestDistributionPortSurvivesMemberFailover(t *testing.T) {
	b := &fakeNode{fail: func(argv []string) bool {
		return strings.Contains(strings.Join(argv, " "), "10.10.0.10")
	}}
	c := &Client{Bastion: b, ControlPlanes: []string{"10.10.0.10", "10.10.0.13"}, APIPort: 16443}
	if _, err := c.Run(context.Background(), "get", "nodes"); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 3 || b.calls[2][1] != "--server=https://10.10.0.13:16443" {
		t.Fatalf("distribution API port lost during failover: %v", b.calls)
	}
}

// With no member answering, the call fails. It must not fall through
// to a default server: the kubeconfig's default is a fixed member, and
// falling back to it during an outage means every probe silently
// targets the node that is down. The harness went blind exactly that
// way and reported that a node never went NotReady.
func TestNoMemberAnsweringIsAnError(t *testing.T) {
	b := &fakeNode{fail: func([]string) bool { return true }}
	c := &Client{Bastion: b, ControlPlanes: []string{"10.10.0.10", "10.10.0.13"}}

	if _, err := c.Server(context.Background()); err == nil {
		t.Fatal("with every member unreachable, a server was returned anyway")
	}
	if _, err := c.Run(context.Background(), "get", "nodes"); err == nil {
		t.Fatal("a command ran against a member that could not answer")
	}
}

// The probe is authenticated and asks /readyz. /livez passes while a
// returning member's authorizer is still syncing, and an anonymous
// /readyz is 401 on k0s before readiness is consulted at all.
func TestTheProbeIsAnAuthenticatedReadyz(t *testing.T) {
	b := &fakeNode{}
	c := &Client{Bastion: b, ControlPlanes: []string{"10.10.0.10"}}
	if _, err := c.Server(context.Background()); err != nil {
		t.Fatal(err)
	}

	probe := strings.Join(b.calls[0], " ")
	if !strings.Contains(probe, "/readyz") {
		t.Errorf("the probe is not /readyz: %s", probe)
	}
	if strings.Contains(probe, "/livez") {
		t.Error("the probe is /livez, which passes before a member can authorize")
	}
	// No --kubeconfig=/dev/null, no anonymous flag: it runs with the
	// bastion's own credentials.
	if strings.Contains(probe, "anonymous") {
		t.Error("the probe is anonymous, which is 401 on k0s before readiness is consulted")
	}
}

// A manifest is applied with its bytes actually attached. Applying
// from an unattached stdin reads nothing, applies nothing, and
// reports success.
func TestApplyAttachesTheManifest(t *testing.T) {
	b := &fakeNode{}
	c := &Client{Bastion: b, ControlPlanes: []string{"10.10.0.10"}}

	if err := c.Apply(context.Background(), []byte("kind: Pod\n")); err != nil {
		t.Fatal(err)
	}
	if b.stdin != "kind: Pod\n" {
		t.Errorf("the manifest did not reach the command: %q", b.stdin)
	}
}

type fakeNode struct {
	calls [][]string
	stdin string
	fail  func(argv []string) bool
}

func (n *fakeNode) Name() string { return "bastion" }

func (n *fakeNode) Exec(ctx context.Context, argv ...string) ([]byte, error) {
	n.calls = append(n.calls, argv)
	if n.fail != nil && n.fail(argv) {
		return nil, errors.New("unreachable")
	}
	return nil, nil
}

func (n *fakeNode) Pipe(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		n.stdin = string(b)
	}
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

// Interface is what this node calls the lab's nth link. A fake stands
// in for a container, which calls it what the topology does.
func (n *fakeNode) Interface(nth int) string { return "eth" + strconv.Itoa(nth+1) }
