// Package k0s builds the k0s site.
//
// The distribution this product is written around, and the one that
// makes the point about who balances the API path: k0s ships
// nodeLocalLoadBalancing on every worker's own loopback
// across all the control planes. So this builder carries no
// forwarder, unlike kubeadm's, and the invariant is checked against
// the file k0s's own kubelet reads rather than one this harness wrote.
//
// k0s installs itself as a systemd unit, so a reboot row's start
// brings the node back through the distribution's own supervision —
// which is exactly what those rows exist to prove.
package k0s

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
	"github.com/appmana/labcontainers/pkg/artifact"
	shared "github.com/appmana/labcontainers/pkg/kubernetes/k0s"
	native "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() { cluster.Register(Builder{}) }

// Version is the release this site runs. Pinned: a distribution that
// changes under the matrix makes two runs incomparable.
const Version = "v1.36.2+k0s.0"

// Builder builds a k0s site.
type Builder struct{}

func (Builder) Name() string { return "k0s" }

// CRIEndpoint is k0s's own containerd socket. A crictl aimed at the
// wrong one sees no containers, which reads as every path being
// broken at once.
func (Builder) CRIEndpoint() string { return "unix:///run/k0s/containerd.sock" }

// NeedsNodeImage is false: k0s installs its own runtime, so a
// machine needs nothing underneath it.
func (Builder) NeedsNodeImage() bool { return false }

// ImportArgs goes through k0s's own ctr, because k0s brings its own
// containerd and does not share the one on the node's PATH. An image
// imported into the wrong one is invisible to the kubelet that needs
// it.
func (Builder) ImportArgs() []string {
	return []string{"k0s", "ctr", "images", "import", "-"}
}

// Build carries the binary in, writes each node's config, and starts
// the controllers and then the workers.
func (b Builder) Build(ctx context.Context, d cluster.Deps) error {
	cps := d.Topology.NodesInRole(lab.ControlPlane)
	if len(cps) == 0 {
		return fmt.Errorf("the topology has no control planes")
	}
	if _, err := calicoConfig(d.Network, d.K0sCalicoMTU, d.K0sCalicoManagedAddresses); err != nil {
		return err
	}
	binary, err := b.binary(ctx, d)
	if err != nil {
		return err
	}
	for _, n := range cluster.SiteNodes(d.Topology) {
		if err := d.Rig.Node(n.Name).Put(ctx, bytes.NewReader(binary),
			"/usr/local/bin/k0s", 0o755); err != nil {
			return fmt.Errorf("carrying k0s onto %s: %w", n.Name, err)
		}
	}

	// The first controller, which is also a worker: a row that places
	// tunnels on a control plane needs a kubelet there.
	first := cps[0]
	if err := b.config(ctx, d, first); err != nil {
		return err
	}
	if err := b.start(ctx, d, first, "controller", "--enable-worker", "-c", "/etc/k0s/k0s.yaml",
		siteKubeletArgs(d.Network, first.Address(lab.LANSegment))); err != nil {
		return err
	}
	if err := b.waitForAPI(ctx, d, first, 5*time.Minute); err != nil {
		return err
	}

	kubeconfig, err := d.Rig.Node(first.Name).Exec(ctx, "k0s", "kubeconfig", "admin")
	if err != nil {
		return fmt.Errorf("reading the kubeconfig: %w", err)
	}
	if err := d.Kube.Install(ctx, kubeconfig); err != nil {
		return err
	}

	for _, n := range cps[1:] {
		if err := b.config(ctx, d, n); err != nil {
			return err
		}
		if err := b.join(ctx, d, first, n, "controller",
			"controller", "--enable-worker", "--token-file", "/etc/k0s/token", "-c", "/etc/k0s/k0s.yaml",
			siteKubeletArgs(d.Network, n.Address(lab.LANSegment))); err != nil {
			return err
		}
		// Adding an etcd member changes quorum. Starting the next join before
		// this member serves its API raced token creation against that change
		// on the real VM rig (etcdserver: request timed out).
		if err := b.waitForAPI(ctx, d, n, 5*time.Minute); err != nil {
			return err
		}
	}
	for _, n := range d.Topology.NodesInRole(lab.Worker) {
		if err := b.join(ctx, d, first, n, "worker",
			"worker", "--token-file", "/etc/k0s/token",
			siteKubeletArgs(d.Network, n.Address(lab.LANSegment))); err != nil {
			return err
		}
	}
	return nil
}

// config writes one node's k0s.yaml.
func (b Builder) config(ctx context.Context, d cluster.Deps, n lab.Node) error {
	calicoSettings, err := calicoConfig(d.Network, d.K0sCalicoMTU, d.K0sCalicoManagedAddresses)
	if err != nil {
		return err
	}
	addr := n.Address(lab.LANSegment)
	sans := []string{"127.0.0.1"}
	for _, cp := range d.Topology.NodesInRole(lab.ControlPlane) {
		sans = append(sans, cp.Address(lab.LANSegment))
	}
	for _, cp := range d.Topology.NodesInRole(lab.ControlPlane) {
		sans = append(sans, cp.Name)
	}

	// Supported profiles select k0s's bundled provider; the native default
	// is Kube-router. Calico remains an explicit supported profile.
	provider := networkProvider(d.Network)

	config := &native.ClusterConfig{
		TypeMeta:   metav1.TypeMeta{APIVersion: native.ClusterConfigAPIVersion, Kind: native.ClusterConfigKind},
		ObjectMeta: metav1.ObjectMeta{Name: "k0s"},
		Spec: &native.ClusterSpec{
			API:     &native.APISpec{Address: addr, SANs: sans},
			Storage: &native.StorageSpec{Type: native.EtcdStorageType, Etcd: &native.EtcdConfig{PeerAddress: addr}},
			Network: &native.Network{
				Provider: provider, Calico: calicoSettings, PodCIDR: d.PodCIDR, ServiceCIDR: d.SvcCIDR,
				// k0s's Traefik backend supports both Linux and Windows.
				NodeLocalLoadBalancing: &native.NodeLocalLoadBalancing{Enabled: true, Type: native.NllbTypeTraefik},
				// Container rows cannot raise the host's nf_conntrack_max.
				KubeProxy: &native.KubeProxy{ExtraArgs: map[string]string{"conntrack-max-per-core": "0"}},
			},
		},
	}
	return shared.WriteConfig(ctx, d.Rig.Node(n.Name), "/etc/k0s/k0s.yaml", config)
}

// join mints a token on the first controller and starts the node with
// it.
func (b Builder) join(ctx context.Context, d cluster.Deps, first, n lab.Node, role string, args ...string) error {
	node := d.Rig.Node(n.Name)
	if _, err := node.Exec(ctx, "test", "-f", "/etc/systemd/system/k0sworker.service"); err == nil {
		return nil
	}
	if _, err := node.Exec(ctx, "test", "-f", "/etc/systemd/system/k0scontroller.service"); err == nil {
		return nil
	}

	// The token is minted through the first controller's status
	// socket, so it has to still be serving: a controller that has
	// just been joined can restart the one that admitted it.
	if err := b.waitForAPI(ctx, d, first, 3*time.Minute); err != nil {
		return err
	}
	token, err := d.Rig.Node(first.Name).Exec(ctx, "k0s", "token", "create", "--role", role)
	if err != nil {
		return fmt.Errorf("minting a %s token: %w", role, err)
	}
	clean := strings.TrimSpace(strings.ReplaceAll(string(token), "\r", ""))
	if clean == "" {
		return fmt.Errorf("k0s printed no %s token", role)
	}
	if _, err := node.Exec(ctx, "mkdir", "-p", "/etc/k0s"); err != nil {
		return err
	}
	if err := node.Put(ctx, strings.NewReader(clean), "/etc/k0s/token", 0o600); err != nil {
		return err
	}
	return b.start(ctx, d, n, args...)
}

// start installs k0s as a unit and starts it.
//
// As a unit, because a reboot row brings the node back through the
// distribution's own supervision, and a node started any other way
// would prove this harness can restart it.
func (b Builder) start(ctx context.Context, d cluster.Deps, n lab.Node, args ...string) error {
	node := d.Rig.Node(n.Name)
	installed := false
	for _, unit := range []string{"k0scontroller", "k0sworker"} {
		if _, err := node.Exec(ctx, "test", "-f", "/etc/systemd/system/"+unit+".service"); err == nil {
			installed = true
		}
	}
	if !installed {
		if err := shared.Install(ctx, node, args...); err != nil {
			return err
		}
	}
	out, err := node.Exec(ctx, "k0s", "start")
	if err != nil {
		return fmt.Errorf("%s: k0s start: %w: %s", n.Name, err, out)
	}
	return nil
}

// waitForAPI blocks until the first controller is actually serving.
//
// Asks k0s itself, and then for the kubeconfig. "k0s kubeconfig
// admin" alone is not a readiness probe: it can answer from what is
// on disk before the controller is up, and then the next thing that
// needs a running k0s — minting a join token, which goes through
// /run/k0s/status.sock — fails with a connection reset that reads as
// the distribution being broken rather than as not started yet.
func (b Builder) waitForAPI(ctx context.Context, d cluster.Deps, n lab.Node, within time.Duration) error {
	return wait.Until(ctx, within, n.Name+"'s k0s never served a ready API", func(ctx context.Context) error {
		return shared.Ready(ctx, d.Rig.Node(n.Name))
	})
}

// KubeletInvariant checks that no kubelet depends on another node's
// survival.
//
// Read from the file k0s's own kubelet loads, which nllb writes under
// /run/k0s/nllb and aims at the load balancer on the node's own loopback,
// k0s's choice of address family included. /var/lib/k0s/kubelet.conf
// keeps the join-time server forever and proves nothing about the
// running node — reading it is how a harness concludes a node is
// balanced when it is pinned.
func (b Builder) KubeletInvariant(ctx context.Context, d cluster.Deps) error {
	var checked int
	for _, n := range cluster.SiteNodes(d.Topology) {
		// With a deadline: nllb writes this file when the node's load balancer
		// comes up, which is after k0s starts and after this builder
		// returns. Reading it the instant the site is built finds
		// nothing and says the node has no balancer, when what it has
		// is a balancer that is still starting.
		out, err := b.readWithDeadline(ctx, d, n, 3*time.Minute)
		if err != nil {
			// A controller that is also a worker may keep its own
			// arrangement; nllb's own documentation says as much. What
			// must not happen is a worker pinned to a member.
			if n.Role == lab.ControlPlane {
				continue
			}
			return fmt.Errorf("reading %s's nllb kubeconfig: %w", n.Name, err)
		}
		server := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "server:"))
		switch server {
		case "https://[::1]:7443", "https://127.0.0.1:7443":
		default:
			return fmt.Errorf("%s's kubelet dials %s, not its own node-local load balancer: "+
				"that member's death would strand it with quorum intact", n.Name, server)
		}
		checked++
	}
	if checked == 0 {
		return fmt.Errorf("no node was checked, so this invariant proved nothing")
	}
	return nil
}

// readWithDeadline reads the server line from a node's nllb
// kubeconfig, waiting for the file to exist.
func (b Builder) readWithDeadline(ctx context.Context, d cluster.Deps, n lab.Node, within time.Duration) ([]byte, error) {
	var out []byte
	err := wait.Until(ctx, within, "nllb wrote no kubeconfig on "+n.Name, func(ctx context.Context) error {
		var err error
		out, err = d.Rig.Node(n.Name).Exec(ctx, "sh", "-c",
			"grep -o 'server: .*' /run/k0s/nllb/kubeconfig.yaml")
		return err
	})
	return out, err
}

// binary consumes a prepared, content-pinned artifact. In particular, a missing
// fork build must never silently fall back to an upstream release download.
func (b Builder) binary(ctx context.Context, d cluster.Deps) ([]byte, error) {
	body, err := artifact.ReadFile(ctx, d.K0sBinary, d.K0sBinarySHA256)
	if err != nil {
		return nil, fmt.Errorf("reading prepared k0s binary: %w", err)
	}
	return body, nil
}

// Reuse verifies the installed version and distribution-owned network.
func (Builder) Reuse(ctx context.Context, d cluster.Deps) error {
	for _, node := range cluster.SiteNodes(d.Topology) {
		version, err := d.Rig.Node(node.Name).Exec(ctx, "k0s", "version")
		if err != nil || strings.TrimSpace(string(version)) != Version {
			return fmt.Errorf("%s k0s version does not match %s", node.Name, Version)
		}
		if node.Role == lab.ControlPlane {
			config, err := d.Rig.Node(node.Name).Exec(ctx, "cat", "/etc/k0s/k0s.yaml")
			if err != nil || !strings.Contains(string(config), "provider: "+networkProvider(d.Network)) {
				return fmt.Errorf("%s network does not match requested profile", node.Name)
			}
			if d.Network == "calico-site-bgp" {
				if err := verifySiteBGPConfig(config); err != nil {
					return fmt.Errorf("%s: %w", node.Name, err)
				}
			}

		}
	}
	return nil
}
