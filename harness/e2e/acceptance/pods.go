package acceptance

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/check"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// NamespaceLabel marks the namespaces this command creates, and only
// those are ever deleted by it.
const NamespaceLabel = "cloud-provisioning.appmana.com/acceptance"

// Pods runs the reachability matrix's probes on a real cluster through
// the Kubernetes API: one probe pod and Service per node in a namespace
// of its own, reached by pod exec. Unlike the lab's prober it needs no
// node access, so it also exercises the API server's exec path to every
// node, remotes included.
type Pods struct {
	Config    *rest.Config
	Client    kubernetes.Interface
	Namespace string
	// PullPolicy for the probe image; a real cluster pulls the
	// digest-pinned image itself.
	PullPolicy corev1.PullPolicy
	// UDPProbe is the static UDP prober carried into each probe pod.
	UDPProbe []byte

	mu      sync.Mutex
	carried map[string]bool
}

// Start creates the namespace, a probe pod and Service on each node, and
// waits for them.
func (p *Pods) Start(ctx context.Context, nodes []string, within time.Duration) ([]check.Target, error) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Namespace, Labels: map[string]string{NamespaceLabel: "true"}}}
	if _, err := p.Client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating probe namespace %s: %w", p.Namespace, err)
	}
	for _, node := range nodes {
		pod, service := check.ProbeObjects(node, p.Namespace)
		if p.PullPolicy != "" {
			pod.Spec.Containers[0].ImagePullPolicy = p.PullPolicy
		}
		if _, err := p.Client.CoreV1().Pods(p.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("creating %s's probe: %w", node, err)
		}
		if _, err := p.Client.CoreV1().Services(p.Namespace).Create(ctx, service, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("creating %s's probe Service: %w", node, err)
		}
	}
	deadline := time.Now().Add(within)
	for {
		targets, err := p.targets(ctx, nodes)
		if err == nil {
			return targets, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the probe pods did not all start: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (p *Pods) targets(ctx context.Context, nodes []string) ([]check.Target, error) {
	var out []check.Target
	for _, node := range nodes {
		pod, err := p.Client.CoreV1().Pods(p.Namespace).Get(ctx, "hc-"+node, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if !podReady(pod) {
			return nil, fmt.Errorf("%s's probe is not ready", node)
		}
		service, err := p.Client.CoreV1().Services(p.Namespace).Get(ctx, "svc-hc-"+node, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		var podIPs []string
		for _, ip := range pod.Status.PodIPs {
			podIPs = append(podIPs, ip.IP)
		}
		target, err := check.TargetFor(node, p.Namespace, podIPs, service.Spec.ClusterIPs)
		if err != nil {
			return nil, err
		}
		if raw, err := p.exec(ctx, node, nil, "cat", "/sys/class/net/eth0/mtu"); err == nil {
			target.MTU, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		out = append(out, target)
	}
	return out, nil
}

func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// Stop deletes the probe namespace, but only one this command created.
func (p *Pods) Stop(ctx context.Context) error {
	ns, err := p.Client.CoreV1().Namespaces().Get(ctx, p.Namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ns.Labels[NamespaceLabel] != "true" {
		return fmt.Errorf("namespace %s is not one this command created; leaving it", p.Namespace)
	}
	uid := ns.UID
	return p.Client.CoreV1().Namespaces().Delete(ctx, p.Namespace, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
}

// exec runs argv in node's probe container through the API server.
func (p *Pods) exec(ctx context.Context, node string, stdin []byte, argv ...string) ([]byte, error) {
	req := p.Client.CoreV1().RESTClient().Post().Resource("pods").Namespace(p.Namespace).Name("hc-"+node).
		SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: "serve", Command: argv, Stdin: stdin != nil, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(p.Config, "POST", req.URL())
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	opts := remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}
	if stdin != nil {
		opts.Stdin = bytes.NewReader(stdin)
	}
	if err := executor.StreamWithContext(ctx, opts); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s: %v: %w: %s", node, argv, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// HTTPGet fetches a URL from within node's probe pod.
func (p *Pods) HTTPGet(ctx context.Context, node, url string) ([]byte, error) {
	return p.exec(ctx, node, nil, "wget", "-q", "-T", "20", "-O", "-", url)
}

// HTTPSize posts check.TransferBytes to url's echo and counts the reply.
func (p *Pods) HTTPSize(ctx context.Context, node, url string) (int64, error) {
	out, err := p.exec(ctx, node, nil, "sh", "-ec", check.TransferCommand, "cldt-transfer", url, check.TransferFile)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: invalid transfer byte count %q", node, out)
	}
	return n, nil
}

// Resolve looks a name up from within node's probe pod.
func (p *Pods) Resolve(ctx context.Context, node, name string) error {
	_, err := p.exec(ctx, node, nil, "nslookup", name)
	return err
}

// Lookup asks cluster DNS for name's addresses in one family.
func (p *Pods) Lookup(ctx context.Context, node, name string, family check.Family) ([]string, error) {
	qtype := "A"
	if family == check.IPv6 {
		qtype = "AAAA"
	}
	out, err := p.exec(ctx, node, nil, "nslookup", "-type="+qtype, name)
	if err != nil {
		return nil, err
	}
	return check.NslookupAnswers(string(out)), nil
}

// UDPEcho runs the carried UDP prober in node's probe pod.
func (p *Pods) UDPEcho(ctx context.Context, node, destination string, payload, tries int, dontFragment bool) (check.UDPReport, error) {
	if len(p.UDPProbe) == 0 {
		return check.UDPReport{}, fmt.Errorf("no UDP prober was given to carry into the probe pods")
	}
	if err := p.carry(ctx, node); err != nil {
		return check.UDPReport{}, err
	}
	argv := check.UDPProbeArgv(destination, payload, tries, dontFragment)
	out, runErr := p.exec(ctx, node, nil, argv...)
	report, err := check.ParseUDPReport(out)
	if err != nil {
		if runErr != nil {
			return check.UDPReport{}, runErr
		}
		return check.UDPReport{}, err
	}
	return report, nil
}

func (p *Pods) carry(ctx context.Context, node string) error {
	p.mu.Lock()
	done := p.carried[node]
	p.mu.Unlock()
	if done {
		return nil
	}
	if _, err := p.exec(ctx, node, p.UDPProbe, "sh", "-ec", check.CarryCommand, "cldt-carry", check.UDPProbePath); err != nil {
		return fmt.Errorf("carrying the UDP prober into %s's probe: %w", node, err)
	}
	p.mu.Lock()
	if p.carried == nil {
		p.carried = map[string]bool{}
	}
	p.carried[node] = true
	p.mu.Unlock()
	return nil
}

// LinuxNodes are the Ready Linux nodes, the matrix's default population.
func LinuxNodes(ctx context.Context, c kubernetes.Interface) ([]string, error) {
	list, err := c.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range list.Items {
		if n.Labels["kubernetes.io/os"] != "linux" {
			continue
		}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				out = append(out, n.Name)
			}
		}
	}
	return out, nil
}

var (
	_ check.Prober         = (*Pods)(nil)
	_ check.TransferProber = (*Pods)(nil)
	_ check.LookupProber   = (*Pods)(nil)
	_ check.UDPProber      = (*Pods)(nil)
)
