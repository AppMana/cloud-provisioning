// Node provisioning for the machine rig.
//
// kubeadm is the one distribution here that does not bring its own
// runtime. k0s, k3s and RKE2 each ship a single binary that installs
// everything below the kubelet; kubeadm expects a node that is
// already a Kubernetes node — containerd, runc, the CNI plugins, a
// kubelet and a supervisor for it — and only then configures a
// control plane on top.
//
// A container rig never had to notice. kindest/node is a Kubernetes
// node image and carries the whole stack, so the builder could reach
// straight for ctr and kubeadm. A cloud image carries none of it, and
// the first thing a machine said was
//
//	ctr: command not found
//
// which is not a broken lab, it is an empty one.
//
// So the platform builds the node, from the same artifacts the
// distribution's own packages contain, at the version the container
// rig's node image ships. Fetched once onto this host and carried in,
// because the lab's nodes reach only what their own edges explain and
// doing it per node per run would be the slowest part of the row.
package kubeadm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// The versions a node is built from. Pinned together: a node whose
// runtime and kubelet drift apart is not the node the other rig runs.
const (
	KubernetesVersion = "v1.34.0"
	ContainerdVersion = "1.7.28"
	RuncVersion       = "v1.2.6"
)

// EnsureStack makes each node a Kubernetes node, where it is not one
// already.
//
// Nodes that already carry kubeadm are left untouched, which is every
// node on the container rig: its image is a node image.
func EnsureStack(ctx context.Context, r rig.Rig, workDir string, nodes []string) error {
	var missing []string
	for _, name := range nodes {
		if _, err := r.Node(name).Exec(ctx, "sh", "-c", "command -v kubeadm"); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	// The plugins every network chains, which this node also has none
	// of; shared with the installers that need them on a node image
	// that already has a runtime.
	if err := cluster.EnsureCNIPlugins(ctx, r, workDir, missing); err != nil {
		return err
	}

	parts, err := fetchStack(ctx, workDir)
	if err != nil {
		return err
	}
	for _, name := range missing {
		if err := installStack(ctx, r.Node(name), parts); err != nil {
			return fmt.Errorf("building %s into a node: %w", name, err)
		}
	}
	return nil
}

// stack is every file a node needs, already on this host.
type stack struct {
	containerd map[string][]byte // basename -> binary, from the release tarball
	runc       []byte
	kubeadm    []byte
	kubelet    []byte
	kubectl    []byte
}

func installStack(ctx context.Context, n rig.Node, s *stack) error {
	for name, body := range s.containerd {
		if err := n.Put(ctx, bytes.NewReader(body), "/usr/local/bin/"+name, 0o755); err != nil {
			return err
		}
	}
	if err := n.Put(ctx, bytes.NewReader(s.runc), "/usr/local/sbin/runc", 0o755); err != nil {
		return err
	}
	for path, body := range map[string][]byte{
		"/usr/local/bin/kubeadm": s.kubeadm,
		"/usr/local/bin/kubelet": s.kubelet,
		"/usr/local/bin/kubectl": s.kubectl,
	} {
		if err := n.Put(ctx, bytes.NewReader(body), path, 0o755); err != nil {
			return err
		}
	}

	// What a container inherited from this host and a machine has to
	// be given: the bridge netfilter path kube-proxy's rules sit on,
	// forwarding, and no swap, which the kubelet refuses to start
	// beside.
	if err := n.Put(ctx, strings.NewReader("overlay\nbr_netfilter\n"),
		"/etc/modules-load.d/kubernetes.conf", 0o644); err != nil {
		return err
	}
	if _, err := n.Exec(ctx, "sh", "-c", "modprobe overlay; modprobe br_netfilter"); err != nil {
		return fmt.Errorf("loading the bridge netfilter modules: %w", err)
	}
	if err := n.Put(ctx, strings.NewReader(
		"net.bridge.bridge-nf-call-iptables = 1\n"+
			"net.bridge.bridge-nf-call-ip6tables = 1\n"+
			"net.ipv4.ip_forward = 1\n"),
		"/etc/sysctl.d/99-kubernetes.conf", 0o644); err != nil {
		return err
	}
	if _, err := n.Exec(ctx, "sysctl", "--system"); err != nil {
		return fmt.Errorf("applying the node's sysctls: %w", err)
	}
	if _, err := n.Exec(ctx, "swapoff", "-a"); err != nil {
		return fmt.Errorf("turning swap off: %w", err)
	}

	// containerd's cgroup driver has to be the one the kubelet uses,
	// and kubeadm's kubelet uses systemd. Mismatched, pods are created
	// and then killed by a cgroup nobody owns, which reads as random
	// eviction.
	if err := n.Put(ctx, strings.NewReader(containerdConfig), "/etc/containerd/config.toml", 0o644); err != nil {
		return err
	}
	if err := n.Put(ctx, strings.NewReader(containerdUnit), "/etc/systemd/system/containerd.service", 0o644); err != nil {
		return err
	}
	if err := n.Put(ctx, strings.NewReader(kubeletUnit), "/etc/systemd/system/kubelet.service", 0o644); err != nil {
		return err
	}
	if err := n.Put(ctx, strings.NewReader(kubeadmDropin),
		"/etc/systemd/system/kubelet.service.d/10-kubeadm.conf", 0o644); err != nil {
		return err
	}
	if _, err := n.Exec(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if _, err := n.Exec(ctx, "systemctl", "enable", "--now", "containerd"); err != nil {
		return fmt.Errorf("starting containerd: %w", err)
	}
	// kubelet is enabled but not started: kubeadm starts it, and a
	// kubelet running before there is anything for it to read
	// crash-loops and clutters every log the row will be read from.
	if _, err := n.Exec(ctx, "systemctl", "enable", "kubelet"); err != nil {
		return fmt.Errorf("enabling the kubelet: %w", err)
	}
	return nil
}

const containerdConfig = `version = 2
[plugins."io.containerd.grpc.v1.cri"]
  sandbox_image = "registry.k8s.io/pause:3.10"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
  SystemdCgroup = true
`

const containerdUnit = `[Unit]
Description=containerd container runtime
After=network.target

[Service]
ExecStart=/usr/local/bin/containerd
Restart=always
RestartSec=5
Delegate=yes
KillMode=process
LimitNOFILE=1048576
TasksMax=infinity

[Install]
WantedBy=multi-user.target
`

const kubeletUnit = `[Unit]
Description=kubelet: the Kubernetes node agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/kubelet
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`

// The drop-in kubeadm itself writes: the kubelet reads its bootstrap
// kubeconfig before a node exists and its real one afterwards, and
// kubeadm writes the rest of the flags into /var/lib/kubelet as it
// goes.
const kubeadmDropin = `[Service]
Environment="KUBELET_KUBECONFIG_ARGS=--bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf"
Environment="KUBELET_CONFIG_ARGS=--config=/var/lib/kubelet/config.yaml"
EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
EnvironmentFile=-/etc/default/kubelet
ExecStart=
ExecStart=/usr/local/bin/kubelet $KUBELET_KUBECONFIG_ARGS $KUBELET_CONFIG_ARGS $KUBELET_KUBEADM_ARGS $KUBELET_EXTRA_ARGS
`

// fetchStack downloads each artifact once and caches it under the
// work directory.
func fetchStack(ctx context.Context, workDir string) (*stack, error) {
	s := &stack{containerd: map[string][]byte{}}

	tarball, err := cached(ctx, workDir, "containerd-"+ContainerdVersion+".tar.gz",
		fmt.Sprintf("https://github.com/containerd/containerd/releases/download/v%[1]s/containerd-%[1]s-linux-amd64.tar.gz",
			ContainerdVersion))
	if err != nil {
		return nil, err
	}
	if err := eachFile(tarball, func(name string, body []byte) {
		s.containerd[path.Base(name)] = body
	}); err != nil {
		return nil, fmt.Errorf("reading the containerd archive: %w", err)
	}
	if len(s.containerd) == 0 {
		return nil, fmt.Errorf("the containerd archive carried no binaries")
	}

	if s.runc, err = cached(ctx, workDir, "runc-"+RuncVersion,
		"https://github.com/opencontainers/runc/releases/download/"+RuncVersion+"/runc.amd64"); err != nil {
		return nil, err
	}
	for name, into := range map[string]*[]byte{
		"kubeadm": &s.kubeadm, "kubelet": &s.kubelet, "kubectl": &s.kubectl,
	} {
		body, err := cached(ctx, workDir, name+"-"+KubernetesVersion,
			"https://dl.k8s.io/release/"+KubernetesVersion+"/bin/linux/amd64/"+name)
		if err != nil {
			return nil, err
		}
		*into = body
	}
	return s, nil
}

func cached(ctx context.Context, workDir, name, url string) ([]byte, error) {
	path := filepath.Join(workDir, name)
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		return body, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: %s", name, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return nil, err
	}
	return body, nil
}

// eachFile walks a gzipped tar, handing every regular file to fn.
func eachFile(archive []byte, fn func(name string, body []byte)) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		fn(header.Name, body)
	}
}
