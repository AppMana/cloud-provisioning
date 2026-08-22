package kubeadm

import (
	"context"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// The forwarder must fan out to every control plane, not to one. A
// forwarder naming a single member is a rename of the problem it
// exists to solve.
func TestTheForwarderFansOutToEveryMember(t *testing.T) {
	r := &fakeRig{}
	d := cluster.Deps{Topology: lab.Default(), Rig: r, PodCIDR: "10.244.0.0/16", SvcCIDR: "10.96.0.0/12"}

	if err := (Builder{}).forwarders(context.Background(), d, cluster.ControlPlaneAddresses(d.Topology)); err != nil {
		t.Fatal(err)
	}

	conf := r.fileOn("w1", "/etc/api-proxy/nginx.conf")
	for _, addr := range cluster.ControlPlaneAddresses(lab.Default()) {
		if !strings.Contains(conf, addr+":6443") {
			t.Errorf("the forwarder on w1 does not carry %s: %s", addr, conf)
		}
	}
	if n := strings.Count(conf, ":6443"); n != 3 {
		t.Errorf("the forwarder names %d members, want 3", n)
	}

	// And it must be a static pod, because kubelet has to be able to
	// start it with no API access at all — during exactly the outage
	// it exists to survive.
	manifest := r.fileOn("w1", "/etc/kubernetes/manifests/api-proxy.yaml")
	if !strings.Contains(manifest, "hostNetwork: true") {
		t.Error("the forwarder is not on the host network, so a loopback listener reaches nothing")
	}
	if !strings.Contains(manifest, "imagePullPolicy: Never") {
		t.Error("the forwarder would try to pull, and the site has no route to a registry")
	}

	// Every site node, not just the workers: a control plane that
	// loses its own API server must still reach the others.
	for _, n := range cluster.SiteNodes(lab.Default()) {
		if r.fileOn(n.Name, "/etc/api-proxy/nginx.conf") == "" {
			t.Errorf("%s has no forwarder", n.Name)
		}
	}
}

// controlPlaneEndpoint must be the loopback, and the certificate must
// cover it. Naming a member instead pins every kubelet kubeadm
// configures to that member for the cluster's life.
func TestTheClusterEndpointIsTheNodesOwnLoopback(t *testing.T) {
	r := &fakeRig{failExec: true} // so init writes its config then stops
	d := cluster.Deps{Topology: lab.Default(), Rig: r, PodCIDR: "10.244.0.0/16", SvcCIDR: "10.96.0.0/12"}

	_ = (Builder{}).init(context.Background(), d,
		lab.Default().MustNode("cp"), cluster.ControlPlaneAddresses(lab.Default()))

	config := r.fileOn("cp", "/tmp/init.yaml")
	if !strings.Contains(config, "controlPlaneEndpoint: 127.0.0.1:7445") {
		t.Errorf("the cluster endpoint is not the node's own forwarder:\n%s", config)
	}
	if !strings.Contains(config, "127.0.0.1") || !strings.Contains(config, "certSANs") {
		t.Error("loopback is not in the certificate SANs, so a client cannot verify the address it dialled")
	}
	for _, addr := range cluster.ControlPlaneAddresses(lab.Default()) {
		if !strings.Contains(config, addr) {
			t.Errorf("%s is not in the SANs, so the bastion cannot dial it directly", addr)
		}
	}
	// kube-proxy must not try to raise nf_conntrack_max: /proc/sys is
	// not writable from a container, so it exits, nothing translates a
	// service address, and the network's own pods never start.
	if !strings.Contains(config, "maxPerCore: 0") {
		t.Error("kube-proxy would raise nf_conntrack_max, which it cannot do here")
	}
}

// The invariant must fail when a kubelet names a member, and must not
// pass by checking nothing.
func TestTheKubeletInvariantCatchesAPinnedNode(t *testing.T) {
	d := cluster.Deps{Topology: lab.Default()}

	d.Rig = &fakeRig{execOut: map[string]string{
		"": "server: https://127.0.0.1:7445",
	}}
	if err := (Builder{}).KubeletInvariant(context.Background(), d); err != nil {
		t.Errorf("a correctly configured site failed the invariant: %v", err)
	}

	// A control plane dialling its OWN API server is kubeadm's choice
	// during init and is no cross-node dependency: that server dies
	// only when the node does.
	d.Rig = &fakeRig{execOut: map[string]string{
		"":   "server: https://127.0.0.1:7445",
		"cp": "server: https://10.10.0.10:6443",
	}}
	if err := (Builder{}).KubeletInvariant(context.Background(), d); err != nil {
		t.Errorf("a control plane dialling its own API server failed the invariant: %v", err)
	}

	// A worker dialling a member is the real failure: it inherited a
	// single point of failure from whichever member its join went
	// through.
	d.Rig = &fakeRig{execOut: map[string]string{
		"":   "server: https://127.0.0.1:7445",
		"w2": "server: https://10.10.0.10:6443",
	}}
	err := (Builder{}).KubeletInvariant(context.Background(), d)
	if err == nil {
		t.Fatal("a worker pinned to one member passed the invariant")
	}
	if !strings.Contains(err.Error(), "w2") || !strings.Contains(err.Error(), "quorum") {
		t.Errorf("the failure does not say which node or what it costs: %v", err)
	}

	// A topology with no site nodes must not pass.
	empty := cluster.Deps{Topology: lab.Topology{Name: "empty"}, Rig: &fakeRig{}}
	if err := (Builder{}).KubeletInvariant(context.Background(), empty); err == nil {
		t.Error("an empty site passed the invariant, having checked nothing")
	}
}

type fakeRig struct {
	mu       sync.Mutex
	files    map[string]string
	execOut  map[string]string
	failExec bool
}

func (f *fakeRig) Kind() string                   { return "fake" }
func (f *fakeRig) Up(ctx context.Context) error   { return nil }
func (f *fakeRig) Down(ctx context.Context) error { return nil }
func (f *fakeRig) Node(name string) rig.Node      { return &fakeNode{rig: f, name: name} }

func (f *fakeRig) fileOn(node, path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[node+":"+path]
}

type fakeNode struct {
	rig  *fakeRig
	name string
}

func (n *fakeNode) Name() string { return n.name }

func (n *fakeNode) Exec(ctx context.Context, argv ...string) ([]byte, error) {
	n.rig.mu.Lock()
	defer n.rig.mu.Unlock()
	if out, ok := n.rig.execOut[n.name]; ok {
		return []byte(out), nil
	}
	if out, ok := n.rig.execOut[""]; ok {
		return []byte(out), nil
	}
	if n.rig.failExec {
		return nil, errFake
	}
	return nil, nil
}

func (n *fakeNode) Pipe(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	return n.Exec(ctx, argv...)
}

func (n *fakeNode) Put(ctx context.Context, src io.Reader, dst string, mode fs.FileMode) error {
	b, _ := io.ReadAll(src)
	n.rig.mu.Lock()
	defer n.rig.mu.Unlock()
	if n.rig.files == nil {
		n.rig.files = map[string]string{}
	}
	n.rig.files[n.name+":"+dst] = string(b)
	return nil
}

func (n *fakeNode) Cut(ctx context.Context) error                          { return nil }
func (n *fakeNode) Restore(ctx context.Context) error                      { return nil }
func (n *fakeNode) Kill(ctx context.Context) error                         { return nil }
func (n *fakeNode) Boot(ctx context.Context) error                         { return nil }
func (n *fakeNode) Userdata(ctx context.Context, cloudConfig []byte) error { return nil }

var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "fake" }

// Interface is what this node calls the lab's nth link. A fake stands
// in for a container, which calls it what the topology does.
func (n *fakeNode) Interface(nth int) string { return "eth" + strconv.Itoa(nth+1) }
