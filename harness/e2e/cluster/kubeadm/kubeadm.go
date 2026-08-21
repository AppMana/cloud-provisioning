// Package kubeadm builds the kubeadm site: three control planes and
// two workers, and no VIP.
//
// There is no address that moves between nodes. Every node holds all
// three control-plane addresses and reaches "the API server" through
// its own loopback: a static-pod TCP forwarder on 127.0.0.1:7445,
// written before kubeadm ever runs, because kubelet starts static
// pods from disk with no API access at all. controlPlaneEndpoint
// names that loopback, so every kubelet.conf kubeadm writes points at
// the node's own forwarder and no single member's death strands any
// node.
//
// That is the same shape the remotes get from the dialer's balancer
// and the same shape k0s calls nllb. kubeadm is the one distribution
// that ships nothing of the sort, which is why this file carries a
// forwarder and the others do not.
package kubeadm

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

func init() { cluster.Register(Builder{}) }

// ProxyPort is the node-local forwarder every kubelet dials.
const ProxyPort = 7445

// NginxImage fronts the control planes. Any TCP proxy would do; this
// one is small and already in every mirror.
const NginxImage = "nginx:1.27-alpine"

// Builder builds a kubeadm site.
type Builder struct{}

func (Builder) Name() string { return "kubeadm" }

// CRIEndpoint is containerd's default socket, which is where kubeadm
// leaves it.
func (Builder) CRIEndpoint() string { return "unix:///run/containerd/containerd.sock" }

// Build writes the forwarder onto every site node, initialises the
// first control plane, then joins the rest.
func (b Builder) Build(ctx context.Context, d cluster.Deps) error {
	addrs := cluster.ControlPlaneAddresses(d.Topology)
	if len(addrs) == 0 {
		return fmt.Errorf("the topology has no control planes")
	}

	if err := b.forwarders(ctx, d, addrs); err != nil {
		return err
	}

	cps := d.Topology.NodesInRole(lab.ControlPlane)
	first := cps[0]
	if err := b.init(ctx, d, first, addrs); err != nil {
		return fmt.Errorf("initialising %s: %w", first.Name, err)
	}
	return nil
}

// forwarders puts the node-local TCP proxy on every site node.
//
// The image is carried in from this host because the site pulls
// nothing: it has no route to a registry, which is the point of it
// being a site.
func (b Builder) forwarders(ctx context.Context, d cluster.Deps, addrs []string) error {
	var upstreams strings.Builder
	for _, a := range addrs {
		fmt.Fprintf(&upstreams, "    server %s:6443 max_fails=1 fail_timeout=5s;\n", a)
	}
	conf := fmt.Sprintf(`worker_processes 1;
events { worker_connections 256; }
stream {
  upstream api {
%s  }
  server {
    listen 127.0.0.1:%d;
    proxy_connect_timeout 2s;
    proxy_pass api;
  }
}
`, upstreams.String(), ProxyPort)

	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: api-proxy
  namespace: kube-system
spec:
  hostNetwork: true
  priorityClassName: system-node-critical
  containers:
    - name: nginx
      image: %s
      imagePullPolicy: Never
      command: ["nginx", "-g", "daemon off;", "-c", "/etc/api-proxy/nginx.conf"]
      volumeMounts:
        - {name: conf, mountPath: /etc/api-proxy, readOnly: true}
  volumes:
    - name: conf
      hostPath:
        path: /etc/api-proxy
        type: Directory
`, NginxImage)

	for _, n := range cluster.SiteNodes(d.Topology) {
		node := d.Rig.Node(n.Name)
		if _, err := node.Exec(ctx, "mkdir", "-p", "/etc/api-proxy", "/etc/kubernetes/manifests"); err != nil {
			return err
		}
		if err := node.Put(ctx, strings.NewReader(conf), "/etc/api-proxy/nginx.conf", 0o644); err != nil {
			return err
		}
		// kubelet reads /etc/kubernetes/manifests from disk whenever
		// it runs, so the forwarder exists on a node before, during
		// and after any control plane's death, depending on nothing
		// that the API it fronts provides.
		if err := node.Put(ctx, strings.NewReader(manifest),
			"/etc/kubernetes/manifests/api-proxy.yaml", 0o644); err != nil {
			return err
		}
	}
	return nil
}

// init runs kubeadm init on the first control plane.
func (b Builder) init(ctx context.Context, d cluster.Deps, first lab.Node, addrs []string) error {
	sans := append([]string{"127.0.0.1"}, addrs...)
	for _, n := range d.Topology.NodesInRole(lab.ControlPlane) {
		sans = append(sans, n.Name)
	}

	config := fmt.Sprintf(`apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint:
  advertiseAddress: %s
  bindPort: 6443
nodeRegistration:
  kubeletExtraArgs:
    - {name: node-ip, value: %s}
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
networking:
  podSubnet: %s
  serviceSubnet: %s
# Every kubelet.conf kubeadm writes points here, which on any node is
# that node's own path to whichever member is alive. Loopback in every
# API server's SANs is what lets a client verify the certificate for
# the address it dialled; the real addresses are there for the bastion
# and for anything dialling a member directly.
controlPlaneEndpoint: 127.0.0.1:%d
apiServer:
  certSANs: [%s]
---
apiVersion: kubeproxy.config.k8s.io/v1alpha1
kind: KubeProxyConfiguration
conntrack:
  # Left alone. kube-proxy would otherwise raise nf_conntrack_max, and
  # /proc/sys is not writable from a container, so it exits and nothing
  # translates a service address: the network's own pods then cannot
  # reach the API service and never start. The kernel here belongs to
  # the host and its value is the host's to choose.
  maxPerCore: 0
  min: 0
`, first.Address(lab.LANSegment), first.Address(lab.LANSegment),
		d.PodCIDR, d.SvcCIDR, ProxyPort, strings.Join(sans, ", "))

	node := d.Rig.Node(first.Name)
	if _, err := node.Exec(ctx, "test", "-f", "/etc/kubernetes/admin.conf"); err == nil {
		return nil // already initialised
	}
	if err := node.Put(ctx, bytes.NewReader([]byte(config)), "/tmp/init.yaml", 0o644); err != nil {
		return err
	}
	// Preflight inspects the kernel it runs on, which in a container
	// is this host's, so it checks things the node neither owns nor
	// can change. It also objects to a manifests directory that
	// already holds the forwarder, which is there on purpose.
	out, err := node.Exec(ctx, "kubeadm", "init", "--config", "/tmp/init.yaml",
		"--skip-token-print", "--upload-certs", "--ignore-preflight-errors=all")
	if err != nil {
		return fmt.Errorf("%w\n%s", err, tail(out, 30))
	}
	return nil
}

// KubeletInvariant checks that every kubelet dials its own loopback
// forwarder rather than a specific member.
//
// A kubelet pinned to one control plane turns that member's death
// into an outage for a node that had quorum available the whole time,
// and nothing about a healthy cluster reveals the difference — which
// is why it is asserted rather than assumed.
func (b Builder) KubeletInvariant(ctx context.Context, d cluster.Deps) error {
	want := fmt.Sprintf("server: https://127.0.0.1:%d", ProxyPort)
	var checked int
	for _, n := range cluster.SiteNodes(d.Topology) {
		out, err := d.Rig.Node(n.Name).Exec(ctx, "grep", "-o", "server: .*", "/etc/kubernetes/kubelet.conf")
		if err != nil {
			return fmt.Errorf("reading %s's kubelet.conf: %w", n.Name, err)
		}
		got := strings.TrimSpace(string(out))
		if got != want {
			return fmt.Errorf("%s's kubelet dials %q, not its own forwarder (%q): "+
				"one member's death would strand it with quorum intact", n.Name, got, want)
		}
		checked++
	}
	// Zero nodes checked is a failure: an assertion that found nothing
	// to assert on proved nothing.
	if checked == 0 {
		return fmt.Errorf("no site nodes were checked, so this invariant proved nothing")
	}
	return nil
}

func tail(b []byte, lines int) string {
	parts := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}
