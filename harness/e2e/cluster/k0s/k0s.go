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
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/labcontainers/pkg/artifact"
	shared "github.com/appmana/labcontainers/pkg/kubernetes/k0s"
	"github.com/appmana/labcontainers/pkg/rig"
	"github.com/appmana/labcontainers/pkg/wait"
	native "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() { cluster.Register(Builder{}) }

// Version is the release this site runs. Pinned: a distribution that
// changes under the matrix makes two runs incomparable.
const Version = "v1.36.4+k0s.1"

func expectedVersion(d cluster.Deps) string {
	if d.K0sVersion != "" {
		return d.K0sVersion
	}
	return Version
}

func verifyVersion(ctx context.Context, d cluster.Deps, name string) error {
	return shared.VerifyVersion(ctx, d.Rig.Node(name), expectedVersion(d))
}

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

// Build writes each node's configuration and lets k0s.Site carry the binary,
// start the first controller and join the rest.
func (b Builder) Build(ctx context.Context, d cluster.Deps) error {
	if err := ValidateImages(d.Network, d.K0sImages); err != nil {
		return err
	}
	cps := d.Topology.NodesInRole(lab.ControlPlane)
	if len(cps) == 0 {
		return fmt.Errorf("the topology has no control planes")
	}
	if err := validateSiteConfig(d); err != nil {
		return err
	}
	binary, err := b.binary(ctx, d)
	if err != nil {
		return err
	}
	site := shared.Site{Binary: binary, Version: expectedVersion(d), ReadyWithin: 5 * time.Minute}
	// Every controller is also a worker: a row that places tunnels on a
	// control plane needs a kubelet there.
	for _, n := range cps {
		config, err := b.config(d, n)
		if err != nil {
			return err
		}
		args := []string{"--enable-worker", siteKubeletArgs(n.ClusterAddress())}
		if d.Network == BGPDualStack {
			args = bgpDualStackControllerArgs(n)
		}
		site.Members = append(site.Members, shared.Member{Node: d.Rig.Node(n.Name), Role: shared.Controller, Config: config, Args: args})
	}
	for _, n := range d.Topology.NodesInRole(lab.Worker) {
		args := []string{siteKubeletArgs(n.ClusterAddress())}
		if d.Network == BGPDualStack {
			args = bgpDualStackWorkerArgs(n)
		}
		site.Members = append(site.Members, shared.Member{Node: d.Rig.Node(n.Name), Role: shared.Worker, Args: args})
	}
	if err := site.Build(ctx); err != nil {
		return err
	}
	kubeconfig, err := d.Rig.Node(cps[0].Name).Exec(ctx, "k0s", "kubeconfig", "admin")
	if err != nil {
		return fmt.Errorf("reading the kubeconfig: %w", err)
	}
	return d.Kube.Install(ctx, kubeconfig)
}

// validateSiteConfig rejects a configuration before any node is touched.
// The dual-stack profile builds its own Calico settings, taking only a
// required MTU; the other Calico profiles go through calicoConfig.
func validateSiteConfig(d cluster.Deps) error {
	if d.Network == BGPDualStack {
		return nil
	}
	_, err := calicoConfig(d.Network, d.K0sCalicoMTU)
	return err
}

// config is one controller's native k0s configuration.
func (b Builder) config(d cluster.Deps, n lab.Node) (*native.ClusterConfig, error) {
	if d.Network == BGPDualStack {
		return bgpDualStackConfig(d, n)
	}
	calicoSettings, err := calicoConfig(d.Network, d.K0sCalicoMTU)
	if err != nil {
		return nil, err
	}
	addr := n.ClusterAddress()
	sans := []string{"127.0.0.1"}
	for _, cp := range d.Topology.NodesInRole(lab.ControlPlane) {
		sans = append(sans, cp.ClusterAddress())
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
			Images:  d.K0sImages.DeepCopy(),
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
	return config, nil
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
		if d.Network == BGPDualStack {
			// No node-local balancing: one controller, which every worker
			// dials at its cluster address, as the deployment does.
			if err := singleControllerKubelet(ctx, d, n); err != nil {
				return err
			}
			checked++
			continue
		}
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
	if expectedVersion(d) != Version && d.K0sBinarySHA256 == "" {
		return fmt.Errorf("reusing a k0s fork requires its expected binary SHA256")
	}
	pin, err := installedBinary(ctx, d)
	if err != nil {
		return err
	}
	d.K0sBinarySHA256 = pin
	for _, node := range cluster.SiteNodes(d.Topology) {
		if err := verifyVersion(ctx, d, node.Name); err != nil {
			return err
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
			if d.Network == BGPDualStack {
				if err := verifyBGPDualStackConfig(config, d, node); err != nil {
					return fmt.Errorf("%s: %w", node.Name, err)
				}
			}

		}
	}
	return nil
}

// installedBinary is the digest of the k0s binary every site node carries.
// A command reusing a site is not given the binary the site was built from,
// so the installed one is the pin; a given pin must equal it.
func installedBinary(ctx context.Context, d cluster.Deps) (string, error) {
	var nodes []rig.Commands
	for _, node := range cluster.SiteNodes(d.Topology) {
		nodes = append(nodes, d.Rig.Node(node.Name))
	}
	pin, err := shared.InstalledDigest(ctx, nodes...)
	if err != nil {
		return "", err
	}
	if d.K0sBinarySHA256 != "" && !strings.EqualFold(d.K0sBinarySHA256, pin) {
		return "", fmt.Errorf("site k0s binary %s differs from the given %s", pin, d.K0sBinarySHA256)
	}
	return pin, nil
}
