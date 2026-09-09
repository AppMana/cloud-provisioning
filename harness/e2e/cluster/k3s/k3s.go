// Package k3s builds the k3s site.
//
// k3s is the second distribution that answers the who-balances
// question for itself, and it answers it differently from k0s: an
// agent carries a client-side load balancer over every server it
// learns from the supervisor, listening on its own loopback, so this
// builder carries no forwarder either and the invariant is checked
// against the kubeconfig the agent's own kubelet reads.
//
// Like k0s it installs itself, runtime included, so a machine needs
// nothing built underneath it — which is the difference between these
// distributions and kubeadm, and the reason only kubeadm's builder
// has to make a node first.
package k3s

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

func init() { cluster.Register(Builder{}) }

// Version is the release this site runs, pinned for the same reason
// every other version here is: a distribution that changes under the
// matrix makes two runs incomparable.
const Version = "v1.34.1+k3s1"

// Token is the shared secret servers and agents join with.
//
// Fixed rather than read back from the first server, because reading
// it is a second thing that can be racing: k3s writes node-token when
// it is ready and a join that reads it early gets an empty file and
// fails with an authentication error that says nothing about timing.
const Token = "cldt-k3s-join-token"

// Builder builds a k3s site.
type Builder struct{}

func (Builder) Name() string { return "k3s" }

// CRIEndpoint is k3s's own containerd socket. k3s brings its own, so
// a crictl aimed at the default one sees no containers at all.
func (Builder) CRIEndpoint() string { return "unix:///run/k3s/containerd/containerd.sock" }

// NeedsNodeImage is false: k3s installs its own runtime, so a
// machine needs nothing underneath it.
func (Builder) NeedsNodeImage() bool { return false }

// ImportArgs goes through k3s's bundled ctr, for the same reason: an
// image imported into the runtime on the node's PATH is invisible to
// the kubelet that needs it.
func (Builder) ImportArgs() []string {
	return []string{"k3s", "ctr", "images", "import", "-"}
}

// Build carries the binary in, starts the first server, then joins
// the rest and the agents.
func (b Builder) Build(ctx context.Context, d cluster.Deps) error {
	binary, err := b.binary(ctx, d)
	if err != nil {
		return err
	}
	for _, n := range cluster.SiteNodes(d.Topology) {
		if err := d.Rig.Node(n.Name).Put(ctx, strings.NewReader(string(binary)),
			"/usr/local/bin/k3s", 0o755); err != nil {
			return fmt.Errorf("carrying k3s onto %s: %w", n.Name, err)
		}
	}

	cps := d.Topology.NodesInRole(lab.ControlPlane)
	if len(cps) == 0 {
		return fmt.Errorf("the topology has no control planes")
	}
	first := cps[0]

	if err := b.start(ctx, d, first, "k3s-server", b.serverArgs(d, first, "--cluster-init")); err != nil {
		return err
	}
	if err := b.waitForAPI(ctx, d, first, 5*time.Minute); err != nil {
		return err
	}

	kubeconfig, err := d.Rig.Node(first.Name).Exec(ctx, "cat", "/etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return fmt.Errorf("reading the kubeconfig: %w", err)
	}
	if err := d.Kube.Install(ctx, kubeconfig); err != nil {
		return err
	}

	joinTo := "--server https://" + first.Address(lab.LANSegment) + ":6443"
	for _, n := range cps[1:] {
		if err := b.start(ctx, d, n, "k3s-server", b.serverArgs(d, n, joinTo)); err != nil {
			return err
		}
		// One at a time: k3s's embedded etcd admits a member and then
		// waits for it to be healthy, and two joining at once leaves
		// a cluster that cannot elect.
		if err := b.waitForAPI(ctx, d, n, 5*time.Minute); err != nil {
			return err
		}
	}
	for _, n := range d.Topology.NodesInRole(lab.Worker) {
		args := "agent " + joinTo + " --token " + Token +
			" --node-ip=" + n.Address(lab.LANSegment) +
			" --node-name=" + n.Name
		if err := b.start(ctx, d, n, "k3s-agent", args); err != nil {
			return err
		}
	}
	return nil
}

// serverArgs is one server's command line.
func (b Builder) serverArgs(d cluster.Deps, n lab.Node, joining string) string {
	sans := []string{"127.0.0.1"}
	for _, cp := range d.Topology.NodesInRole(lab.ControlPlane) {
		sans = append(sans, cp.Address(lab.LANSegment), cp.Name)
	}

	args := []string{
		"server", joining,
		"--token", Token,
		"--node-ip=" + n.Address(lab.LANSegment),
		"--node-name=" + n.Name,
		"--cluster-cidr=" + d.PodCIDR,
		"--service-cidr=" + d.SvcCIDR,
		"--write-kubeconfig-mode=644",
	}
	for _, san := range sans {
		args = append(args, "--tls-san="+san)
	}
	// The distribution's own network is flannel. A row that installs
	// one turns it off here rather than layering: two CNIs on one node
	// is not redundancy, it is two owners for every pod's routes.
	if d.Network != "default" {
		args = append(args, "--flannel-backend=none", "--disable-network-policy")
	}
	// Nothing here needs an ingress or a load balancer, and both take
	// addresses the lab has other plans for.
	args = append(args, "--disable=traefik", "--disable=servicelb")
	return strings.Join(args, " ")
}

// start installs k3s as a unit and starts it.
//
// As a unit, because a reboot row brings the node back through the
// distribution's own supervision, and a node started any other way
// would prove only that this harness can restart it.
func (b Builder) start(ctx context.Context, d cluster.Deps, n lab.Node, unit, args string) error {
	node := d.Rig.Node(n.Name)
	if _, err := node.Exec(ctx, "test", "-f", "/etc/systemd/system/"+unit+".service"); err == nil {
		return nil
	}
	service := fmt.Sprintf(`[Unit]
Description=Lightweight Kubernetes (%[1]s)
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/k3s %[2]s
KillMode=process
Delegate=yes
LimitNOFILE=1048576
TasksMax=infinity
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
`, n.Name, args)

	if err := node.Put(ctx, strings.NewReader(service),
		"/etc/systemd/system/"+unit+".service", 0o644); err != nil {
		return fmt.Errorf("writing %s's unit: %w", n.Name, err)
	}
	if _, err := node.Exec(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if _, err := node.Exec(ctx, "systemctl", "enable", "--now", unit); err != nil {
		return fmt.Errorf("starting k3s on %s: %w", n.Name, err)
	}
	return nil
}

// waitForAPI blocks until this server is actually serving.
//
// The kubeconfig on disk is not the answer: k3s writes it early, and
// the next thing that needs a running server then fails with a
// connection reset that reads as the distribution being broken rather
// than as not started yet.
func (b Builder) waitForAPI(ctx context.Context, d cluster.Deps, n lab.Node, within time.Duration) error {
	return wait.Until(ctx, within, n.Name+"'s k3s never served a ready API", func(ctx context.Context) error {
		node := d.Rig.Node(n.Name)
		if _, err := node.Exec(ctx, "test", "-f", "/etc/rancher/k3s/k3s.yaml"); err != nil {
			return fmt.Errorf("no kubeconfig yet: %w", err)
		}
		if _, err := node.Exec(ctx, "k3s", "kubectl", "get", "--raw", "/readyz"); err != nil {
			return fmt.Errorf("readyz: %w", err)
		}
		return nil
	})
}

// KubeletInvariant checks that no kubelet depends on another node's
// survival.
//
// An agent's kubelet reads a kubeconfig k3s writes pointing at the
// agent's own load balancer on loopback, which fans out over every
// server it has learned. Reading the address the agent was joined
// with instead proves nothing: that is the server it bootstrapped
// from, not the one it is using.
func (b Builder) KubeletInvariant(ctx context.Context, d cluster.Deps) error {
	var checked int
	for _, n := range cluster.SiteNodes(d.Topology) {
		if n.Role == lab.ControlPlane {
			// A server runs its own API and its kubelet talks to it
			// locally; it depends on no other node by construction.
			continue
		}
		out, err := b.readWithDeadline(ctx, d, n, 3*time.Minute)
		if err != nil {
			return fmt.Errorf("reading %s's kubelet kubeconfig: %w", n.Name, err)
		}
		server := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "server:"))
		if !strings.Contains(server, "127.0.0.1") && !strings.Contains(server, "[::1]") {
			return fmt.Errorf("%s's kubelet dials %s, not its own load balancer: "+
				"that server's death would strand it with quorum intact", n.Name, server)
		}
		checked++
	}
	if checked == 0 {
		return fmt.Errorf("no node was checked, so this invariant proved nothing")
	}
	return nil
}

// readWithDeadline reads the server line from an agent's kubelet
// kubeconfig, waiting for the file to exist: k3s writes it when the
// agent comes up, which is after this builder returns.
func (b Builder) readWithDeadline(ctx context.Context, d cluster.Deps, n lab.Node, within time.Duration) ([]byte, error) {
	var out []byte
	err := wait.Until(ctx, within, "k3s wrote no kubelet kubeconfig on "+n.Name, func(ctx context.Context) error {
		var err error
		out, err = d.Rig.Node(n.Name).Exec(ctx, "sh", "-c",
			"grep -o 'server: .*' /var/lib/rancher/k3s/agent/kubelet.kubeconfig")
		return err
	})
	return out, err
}

// binary fetches the pinned release once and caches it, because this
// host has a route out and the site's nodes reach only what their own
// edges explain.
func (b Builder) binary(ctx context.Context, d cluster.Deps) ([]byte, error) {
	path := filepath.Join(d.WorkDir, "k3s-"+Version)
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		return body, nil
	}
	url := "https://github.com/k3s-io/k3s/releases/download/" +
		strings.ReplaceAll(Version, "+", "%2B") + "/k3s"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching k3s: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching k3s: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(d.WorkDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return nil, err
	}
	return body, nil
}
