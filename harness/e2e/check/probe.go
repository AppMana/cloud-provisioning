package check

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

// Image the probe pods run. Small, and it has httpd and wget, which
// is all a reachability probe needs.
const Image = "busybox:1.37"

// Port the probe pods serve on.
const Port = 8080

// Pods puts one probe pod on each node and reaches them through each
// node's own container runtime.
//
// Workload probes use the node's container runtime through out-of-band
// management. This keeps pod-network failures distinguishable from failures
// of the API server's kubelet connection. Kubernetes exec and logs require
// a separate management-path check; a passing workload matrix cannot prove them.
type Pods struct {
	Kube *kube.Client
	Rig  rig.Nodes
	// Namespace is unique per run.
	//
	// A fixed name couples this run to the previous run's cleanup
	// having finished, and namespace deletion is allowed to hang: with
	// a node dead ungracefully, an aggregated API served from it makes
	// the namespace controller's discovery fail and every deletion
	// waits. Measured: a previous check's namespace Terminating for a
	// whole wait, and the survivors' check reporting that no checks
	// ran on a healthy network.
	Namespace string
	// CRIEndpoint is where crictl finds this distribution's runtime. A
	// crictl aimed at the wrong socket sees no containers, which reads
	// as every path being broken at once.
	CRIEndpoint string
}

// Manifest renders one probe pod and its service for a node.
//
// The pod serves two things: a small body every ordinary check reads,
// and a large one for the transfer check. The large one is generated
// on the node rather than carried in, because a megabyte in a
// manifest is a megabyte through the API server.
func Manifest(node, namespace string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: hc-%[1]s
  namespace: %[2]s
  labels: {app: hc-%[1]s}
spec:
  nodeSelector: {kubernetes.io/hostname: %[1]s}
  # A provisioned node carries a taint that keeps ordinary workloads
  # off it, and this check is not ordinary.
  tolerations:
    - operator: Exists
  containers:
    - name: serve
      image: %[3]s
      command: ["sh","-c","mkdir -p /tmp/www; printf ok > /tmp/www/index.html; dd if=/dev/zero of=/tmp/www/big bs=1024 count=%[5]d 2>/dev/null; httpd -f -p %[4]d -h /tmp/www"]
      ports: [{containerPort: %[4]d}]
---
apiVersion: v1
kind: Service
metadata:
  name: svc-hc-%[1]s
  namespace: %[2]s
spec:
  selector: {app: hc-%[1]s}
  ports:
    - {name: http, port: %[4]d, targetPort: %[4]d}
`, node, namespace, Image, Port, TransferBytes/1024)
}

// Start creates a pod and a service on every node and waits for them
// to run.
func (p *Pods) Start(ctx context.Context, nodes []string, within time.Duration) ([]Target, error) {
	if _, err := p.Kube.Run(ctx, "create", "namespace", p.Namespace); err != nil {
		// Already there is fine; anything else surfaces when the pods
		// fail to appear.
		_ = err
	}
	for _, node := range nodes {
		if err := p.Kube.Apply(ctx, []byte(Manifest(node, p.Namespace))); err != nil {
			return nil, fmt.Errorf("creating %s's probe: %w", node, err)
		}
	}

	var targets []Target
	err := wait.Until(ctx, within, "the probe pods did not all start", func(ctx context.Context) error {
		var err error
		targets, err = p.targets(ctx, nodes)
		if err != nil {
			return err
		}
		if len(targets) != len(nodes) {
			return fmt.Errorf("%d of %d probes are ready", len(targets), len(nodes))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return targets, nil
}

func (p *Pods) targets(ctx context.Context, nodes []string) ([]Target, error) {
	var out []Target
	for _, node := range nodes {
		// Ready, not merely Running.
		//
		// A pod whose node was killed comes back at a new address, and
		// for a moment the API still carries the old object: phase
		// Running, and the address it had before. Reading that gives
		// every prober a target that no longer exists, and the failure
		// is selective — the service address still resolves to
		// whatever is serving now, so pod checks fail while service
		// checks beside them pass, a pattern that reads exactly like a
		// routing fault. Measured: every site node failing
		// "to remote1 pod" against 10.244.159.0 while the pod had come
		// back at 10.244.159.1.
		ready, err := p.Kube.Get(ctx, p.Namespace, "pod", "hc-"+node,
			`{.status.conditions[?(@.type=="Ready")].status}`)
		if err != nil || ready != "True" {
			return out, fmt.Errorf("%s's probe is not ready (%q)", node, ready)
		}
		podIP, err := p.Kube.Get(ctx, p.Namespace, "pod", "hc-"+node, "{.status.podIP}")
		if err != nil || podIP == "" {
			return out, fmt.Errorf("%s's probe has no address", node)
		}
		svcIP, err := p.Kube.Get(ctx, p.Namespace, "service", "svc-hc-"+node, "{.spec.clusterIP}")
		if err != nil || svcIP == "" {
			return out, fmt.Errorf("%s's probe has no service address", node)
		}
		out = append(out, Target{Node: node, PodIP: podIP, ServiceIP: svcIP})
	}
	return out, nil
}

// Stop removes the namespace without waiting for it to go.
func (p *Pods) Stop(ctx context.Context) {
	_, _ = p.Kube.Run(ctx, "delete", "namespace", p.Namespace, "--wait=false")
}

// HTTPGet fetches a URL from within the probe pod on a node.
func (p *Pods) HTTPGet(ctx context.Context, node, url string) ([]byte, error) {
	cid, err := p.container(ctx, node)
	if err != nil {
		return nil, err
	}
	// -O - writes to standard output, so the body is what comes back
	// and its length is the measurement the transfer check makes.
	out, err := p.crictl(ctx, node, "exec", cid, "wget", "-q", "-T", "20", "-O", "-", url)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HTTPSize receives the full response in the probe pod and returns its byte
// count. The response still traverses the tested pod network; only the count
// traverses serial or SSM. pipefail prevents a failed wget from passing via wc.
func (p *Pods) HTTPSize(ctx context.Context, node, url string) (int64, error) {
	cid, err := p.container(ctx, node)
	if err != nil {
		return 0, err
	}
	out, err := p.crictl(ctx, node, "exec", cid, "sh", "-ec", `set -o pipefail; wget -q -T 20 -O - "$1" | wc -c`, "cldt-transfer", url)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: invalid transfer byte count", node)
	}
	return n, nil
}

// Resolve looks a name up from within the probe pod on a node.
func (p *Pods) Resolve(ctx context.Context, node, name string) error {
	cid, err := p.container(ctx, node)
	if err != nil {
		return err
	}
	_, err = p.crictl(ctx, node, "exec", cid, "nslookup", name)
	return err
}

// container finds the probe pod's container on a node.
//
// Anchored, and scoped to this run's own namespace. crictl's --name
// is a substring regex and kube-apiserver contains "serve", so on a
// control plane the unanchored form can select the API server, whose
// image has neither wget nor nslookup and reads as a broken network.
func (p *Pods) container(ctx context.Context, node string) (string, error) {
	out, err := p.crictl(ctx, node, "ps", "--name", "^serve$",
		"--label", "io.kubernetes.pod.namespace="+p.Namespace, "-q")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	if id == "" {
		return "", fmt.Errorf("%s runs no probe container", node)
	}
	return id, nil
}

func (p *Pods) crictl(ctx context.Context, node string, args ...string) ([]byte, error) {
	argv := []string{"crictl"}
	if p.CRIEndpoint != "" {
		argv = append(argv, "--runtime-endpoint", p.CRIEndpoint)
	}
	argv = append(argv, args...)
	return p.Rig.Node(node).Exec(ctx, argv...)
}

// UniqueNamespace names a run, so that no run waits on another run's
// cleanup.
func UniqueNamespace(seed int64) string {
	return "cloud-provisioning-health-" + strconv.FormatInt(seed, 10)
}

var _ Prober = (*Pods)(nil)
