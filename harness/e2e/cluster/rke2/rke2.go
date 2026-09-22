// Package rke2 builds the RKE2 site.
//
// The third distribution that balances the API path for itself: like
// k3s, an agent carries a client-side load balancer over every server
// it learns from the supervisor, so this builder carries no forwarder
// and the invariant is checked against the kubeconfig the agent's own
// kubelet reads.
//
// A recorded limitation, carried over from the shell harness rather
// than rediscovered. RKE2 runs the reboot rows but not a sustained
// partition of a server node: rke2-server fatals on "leaderelection
// lost", by design, expecting a clean restart; its containerd dies
// with it while the pod shims survive; and the orphaned etcd, its log
// consumer gone, keeps heartbeating raft while its serving paths
// block on a full stderr pipe. Every restart then dies reading the
// datastore, forever, even after the partition heals — recovered only
// by killing the orphaned etcd by hand. Known upstream
// (rancher/rke2 4510, 4479, 7155). k3s embeds etcd in process and
// restarts it with itself, so it passes the same row; power loss
// orphans nothing, which is why the reboot rows stand for RKE2.
package rke2

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
	"github.com/appmana/labcontainers/pkg/artifact"
	shared "github.com/appmana/labcontainers/pkg/kubernetes/rke2"
)

func init() { cluster.Register(Builder{}) }

// Version is the release this site runs, pinned like every other.
const Version = "v1.34.1+rke2r1"

// Token is the shared secret servers and agents join with, fixed for
// the same reason k3s's is: reading one back is a second thing that
// can be racing.
const Token = "cldt-rke2-join-token"

// SupervisorPort is where a joining node dials, which is not the API
// port: RKE2 runs its supervisor separately, and a node pointed at
// 6443 waits for a service that answers something else.
const SupervisorPort = 9345

// ArtifactDir is where a node keeps the release it installs from.
const ArtifactDir = "/root/rke2-artifacts"

// Builder builds an RKE2 site.
type Builder struct{}

func (Builder) Name() string { return "rke2" }

// CRIEndpoint is RKE2's own containerd socket, which it shares a path
// with k3s.
func (Builder) CRIEndpoint() string { return "unix:///run/k3s/containerd/containerd.sock" }

// NeedsNodeImage is false: RKE2 installs its own runtime, so a
// machine needs nothing underneath it.
func (Builder) NeedsNodeImage() bool { return false }

// ImportArgs goes through the ctr RKE2 ships, at the path it ships it,
// against its own socket: the one on the node's PATH, if there is
// one, is a different runtime holding different images.
func (Builder) ImportArgs() []string {
	return []string{"/var/lib/rancher/rke2/bin/ctr",
		"--address", "/run/k3s/containerd/containerd.sock",
		"-n", "k8s.io", "images", "import", "-"}
}

// Build installs the release on every site node, starts the first
// server, then joins the rest and the agents.
func (b Builder) Build(ctx context.Context, d cluster.Deps) error {
	files, err := preparedArtifacts(ctx, d)
	if err != nil {
		return err
	}
	for _, n := range cluster.SiteNodes(d.Topology) {
		if err := b.install(ctx, d, n, files); err != nil {
			return err
		}
	}

	cps := d.Topology.NodesInRole(lab.ControlPlane)
	if len(cps) == 0 {
		return fmt.Errorf("the topology has no control planes")
	}
	first := cps[0]

	if err := b.config(ctx, d, first, ""); err != nil {
		return err
	}
	if err := b.start(ctx, d, first, "rke2-server"); err != nil {
		return err
	}
	if err := b.waitForAPI(ctx, d, first, 10*time.Minute); err != nil {
		return err
	}

	kubeconfig, err := d.Rig.Node(first.Name).Exec(ctx, "cat", "/etc/rancher/rke2/rke2.yaml")
	if err != nil {
		return fmt.Errorf("reading the kubeconfig: %w", err)
	}
	if err := d.Kube.Install(ctx, kubeconfig); err != nil {
		return err
	}

	joinTo := fmt.Sprintf("https://%s:%d", first.Address(lab.LANSegment), SupervisorPort)
	for _, n := range cps[1:] {
		if err := b.config(ctx, d, n, joinTo); err != nil {
			return err
		}
		if err := b.start(ctx, d, n, "rke2-server"); err != nil {
			return err
		}
		// One at a time, as with k3s: etcd admits a member and then
		// waits for it to be healthy, and two joining at once leaves a
		// cluster that cannot elect.
		if err := b.waitForAPI(ctx, d, n, 10*time.Minute); err != nil {
			return err
		}
	}
	for _, n := range d.Topology.NodesInRole(lab.Worker) {
		if err := b.config(ctx, d, n, joinTo); err != nil {
			return err
		}
		if err := b.start(ctx, d, n, "rke2-agent"); err != nil {
			return err
		}
	}
	return nil
}

// install puts the release on a node and runs RKE2's own installer
// against it.
//
// The installer rather than hand-placed binaries, because it is what
// an operator runs and it writes the units, the tmpfiles and the
// policy that go with them. All bytes were verified before touching any node.
// Reuse is not inferred from the presence of an arbitrary executable.
func (b Builder) install(ctx context.Context, d cluster.Deps, n lab.Node, files map[string][]byte) error {
	node := d.Rig.Node(n.Name)
	if _, err := node.Exec(ctx, "mkdir", "-p", ArtifactDir); err != nil {
		return err
	}

	for _, name := range []string{"rke2.linux-amd64.tar.gz", "sha256sum-amd64.txt"} {
		if err := node.Put(ctx, bytes.NewReader(files[name]),
			ArtifactDir+"/"+name, 0o644); err != nil {
			return fmt.Errorf("carrying %s onto %s: %w", name, n.Name, err)
		}
	}
	if err := node.Put(ctx, bytes.NewReader(files["install.sh"]), "/tmp/rke2-install.sh", 0o755); err != nil {
		return err
	}
	return shared.Install(ctx, node, "/tmp/rke2-install.sh",
		"INSTALL_RKE2_ARTIFACT_PATH="+ArtifactDir,
		"INSTALL_RKE2_VERSION="+Version,
		"INSTALL_RKE2_METHOD=tar",
		"INSTALL_RKE2_TYPE="+b.installType(n))
}

// installType is which set of units a node gets.
func (Builder) installType(n lab.Node) string {
	if n.Role == lab.ControlPlane {
		return "server"
	}
	return "agent"
}

// config writes one node's config.yaml.
func (b Builder) config(ctx context.Context, d cluster.Deps, n lab.Node, joining string) error {
	var c strings.Builder
	fmt.Fprintf(&c, "token: %s\n", Token)
	if joining != "" {
		fmt.Fprintf(&c, "server: %s\n", joining)
	}
	fmt.Fprintf(&c, "node-ip: %s\n", n.Address(lab.LANSegment))
	fmt.Fprintf(&c, "node-name: %s\n", n.Name)

	if n.Role == lab.ControlPlane {
		fmt.Fprintf(&c, "cluster-cidr: %s\n", d.PodCIDR)
		fmt.Fprintf(&c, "service-cidr: %s\n", d.SvcCIDR)
		c.WriteString("tls-san:\n  - 127.0.0.1\n")
		for _, cp := range d.Topology.NodesInRole(lab.ControlPlane) {
			fmt.Fprintf(&c, "  - %s\n  - %s\n", cp.Address(lab.LANSegment), cp.Name)
		}
		// The distribution's own network is canal. A row that installs
		// one turns it off here rather than layering: two networks on
		// one node is not redundancy, it is two owners for every pod's
		// routes.
		if d.Network != "default" {
			c.WriteString("cni: none\n")
		}
		// Neither is wanted here, and both take addresses the lab has
		// other plans for.
		c.WriteString("disable:\n  - rke2-ingress-nginx\n  - rke2-metrics-server\n")
	}

	node := d.Rig.Node(n.Name)
	if _, err := node.Exec(ctx, "mkdir", "-p", "/etc/rancher/rke2"); err != nil {
		return err
	}
	return node.Put(ctx, strings.NewReader(c.String()), "/etc/rancher/rke2/config.yaml", 0o644)
}

// start enables the unit RKE2's installer wrote.
func (b Builder) start(ctx context.Context, d cluster.Deps, n lab.Node, unit string) error {
	node := d.Rig.Node(n.Name)
	if _, err := node.Exec(ctx, "systemctl", "enable", "--now", unit); err != nil {
		return fmt.Errorf("starting %s on %s: %w", unit, n.Name, err)
	}
	return nil
}

// waitForAPI blocks until this server is actually serving.
func (b Builder) waitForAPI(ctx context.Context, d cluster.Deps, n lab.Node, within time.Duration) error {
	return wait.Until(ctx, within, n.Name+"'s RKE2 never served a ready API", func(ctx context.Context) error {
		return shared.Ready(ctx, d.Rig.Node(n.Name), "/var/lib/rancher/rke2/bin/kubectl", "/etc/rancher/rke2/rke2.yaml")
	})
}

// KubeletInvariant checks that no kubelet depends on another node's
// survival.
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

func (b Builder) readWithDeadline(ctx context.Context, d cluster.Deps, n lab.Node, within time.Duration) ([]byte, error) {
	var out []byte
	err := wait.Until(ctx, within, "RKE2 wrote no kubelet kubeconfig on "+n.Name, func(ctx context.Context) error {
		var err error
		out, err = d.Rig.Node(n.Name).Exec(ctx, "sh", "-c",
			"grep -o 'server: .*' /var/lib/rancher/rke2/agent/kubelet.kubeconfig")
		return err
	})
	return out, err
}

// preparedArtifacts validates the complete input set before any VM mutation.
// The installer itself is content-pinned, not fetched from get.rke2.io.
func preparedArtifacts(ctx context.Context, d cluster.Deps) (map[string][]byte, error) {
	if d.RKE2ArtifactsDirectory == "" {
		return nil, fmt.Errorf("prepared RKE2ArtifactsDirectory and all three SHA256 pins are required")
	}
	files := make(map[string][]byte)
	for _, input := range []struct{ name, pin string }{
		{"install.sh", d.RKE2InstallerSHA256},
		{"rke2.linux-amd64.tar.gz", d.RKE2ArchiveSHA256},
		{"sha256sum-amd64.txt", d.RKE2ChecksumsSHA256},
	} {
		body, err := artifact.ReadFile(ctx, filepath.Join(d.RKE2ArtifactsDirectory, input.name), input.pin)
		if err != nil {
			return nil, fmt.Errorf("prepared RKE2 %s: %w", input.name, err)
		}
		files[input.name] = body
	}
	return files, nil
}
