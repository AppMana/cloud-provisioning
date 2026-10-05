package check

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Image the probe pods run: the upstream Kubernetes network test image,
// pinned by digest. Its netexec server answers /echo over HTTP (a posted
// body comes back whole, so one request moves TransferBytes each way) and
// echoes UDP on UDPPort, in both families; its Alpine userland carries the
// curl, nslookup and shell the probes run inside it.
const Image = "registry.k8s.io/e2e-test-images/agnhost:2.56@sha256:352a050380078cb2a1c246357a0dfa2fcf243ee416b92ff28b44a01d1b4b0294"

// Port the probe pods serve HTTP on, and UDPPort their UDP echo.
const (
	Port    = 8080
	UDPPort = 8081
)

// TransferFile is the body each probe posts: TransferBytes of x, written
// once when the pod starts. TransferCommand posts it ($2) to an echo URL
// ($1) and counts what comes back; pipefail keeps a failed curl from
// passing through wc.
const (
	TransferFile    = "/tmp/transfer"
	TransferCommand = `set -o pipefail; curl -sS --fail --noproxy '*' --max-time 60 --data-urlencode "msg@$2" "$1" | wc -c`
)

// UDPProbePath is where the UDP prober is carried into a probe container,
// and CarryCommand writes stdin there ($1) atomically.
const (
	UDPProbePath = "/tmp/udpprobe"
	CarryCommand = `cat > "$1.new"; chmod 0755 "$1.new"; mv "$1.new" "$1"`
)

// UDPProbeArgv is the prober's command line for one size.
func UDPProbeArgv(destination string, payload, tries int, dontFragment bool) []string {
	argv := []string{UDPProbePath, "-destination", destination,
		"-payload-bytes", strconv.Itoa(payload), "-tries", strconv.Itoa(tries)}
	if dontFragment {
		argv = append(argv, "-dont-fragment")
	}
	return argv
}

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
	// UDPProbe is the static UDP prober (cmd/udpprobe) carried into each
	// probe container before its first UDP check. Empty disables UDP.
	UDPProbe []byte

	mu      sync.Mutex
	carried map[string]bool // container IDs holding the UDP prober
}

// ProbeObjects constructs native Kubernetes resources; callers never render YAML.
// Probe images are preloaded by the harness, not fetched through an undeclared
// registry path while measuring reachability.
func ProbeObjects(node, namespace string) (*corev1.Pod, *corev1.Service) {
	name := "hc-" + node
	dualStack := corev1.IPFamilyPolicyPreferDualStack
	pod := &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app": name}},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"kubernetes.io/hostname": node},
			Tolerations:  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name: "serve", Image: Image, ImagePullPolicy: corev1.PullNever,
				Command: []string{"sh", "-c", fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x > %s; exec /agnhost netexec --http-port=%d --udp-port=%d", TransferBytes, TransferFile, Port, UDPPort)},
				Ports: []corev1.ContainerPort{
					{Name: "http", ContainerPort: Port, Protocol: corev1.ProtocolTCP},
					{Name: "udp", ContainerPort: UDPPort, Protocol: corev1.ProtocolUDP},
				},
			}},
		},
	}
	service := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: "svc-" + name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector:       map[string]string{"app": name},
			IPFamilyPolicy: &dualStack,
			Ports: []corev1.ServicePort{
				{Name: "http", Port: Port, TargetPort: intstr.FromInt(Port), Protocol: corev1.ProtocolTCP},
				{Name: "udp", Port: UDPPort, TargetPort: intstr.FromInt(UDPPort), Protocol: corev1.ProtocolUDP},
			},
		},
	}
	return pod, service
}

// Prepare pulls the probe image by digest into each node's runtime, so the
// probe pods start from it without fetching anything while they measure.
// A by-digest pull through the runtime records the digest the pod names,
// which an exported and reimported copy would not.
func (p *Pods) Prepare(ctx context.Context, nodes []string) error {
	for _, node := range nodes {
		if _, err := p.crictl(ctx, node, "pull", Image); err != nil {
			return fmt.Errorf("pulling the probe image onto %s: %w", node, err)
		}
	}
	return nil
}

// BuildUDPProbe compiles the static UDP prober the probe pods run, from the
// harness module at moduleDir, for linux/amd64 nodes.
func BuildUDPProbe(ctx context.Context, moduleDir, workDir string) ([]byte, error) {
	out, err := filepath.Abs(filepath.Join(workDir, "binaries", "udpprobe-linux-amd64"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./cmd/udpprobe")
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if raw, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("building the UDP prober: %w: %s", err, raw)
	}
	return os.ReadFile(out)
}

// Start creates a pod and a service on every node and waits for them
// to run.
func (p *Pods) Start(ctx context.Context, nodes []string, within time.Duration) ([]Target, error) {
	if err := p.Kube.ApplyObjects(ctx, &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: p.Namespace},
	}); err != nil {
		return nil, fmt.Errorf("creating probe namespace: %w", err)
	}
	for _, node := range nodes {
		pod, service := ProbeObjects(node, p.Namespace)
		if err := p.Kube.ApplyObjects(ctx, pod, service); err != nil {
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
		podIPs, err := p.Kube.Get(ctx, p.Namespace, "pod", "hc-"+node, "{.status.podIPs[*].ip}")
		if err != nil || strings.TrimSpace(podIPs) == "" {
			return out, fmt.Errorf("%s's probe has no address", node)
		}
		svcIPs, err := p.Kube.Get(ctx, p.Namespace, "service", "svc-hc-"+node, "{.spec.clusterIPs[*]}")
		if err != nil || strings.TrimSpace(svcIPs) == "" {
			return out, fmt.Errorf("%s's probe has no service address", node)
		}
		target, err := TargetFor(node, p.Namespace, strings.Fields(podIPs), strings.Fields(svcIPs))
		if err != nil {
			return out, err
		}
		// The pod's own MTU, read inside it: the size its network
		// promises to carry, which the UDP probes are sized around.
		if cid, err := p.container(ctx, node); err == nil {
			if raw, err := p.crictl(ctx, node, "exec", cid, "cat", "/sys/class/net/eth0/mtu"); err == nil {
				target.MTU, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			}
		}
		out = append(out, target)
	}
	return out, nil
}

// TargetFor sorts a probe's addresses by family. The pod and its Service
// must agree on which families they have: a dual-stack pod behind a
// single-stack Service, or the reverse, is a cluster that is half
// configured, and measuring only the half that works would hide it.
func TargetFor(node, namespace string, podIPs, serviceIPs []string) (Target, error) {
	t := Target{Node: node, ServiceName: "svc-hc-" + node + "." + namespace + ".svc.cluster.local"}
	assign := func(addrs []string, v4, v6 *string, what string) error {
		for _, a := range addrs {
			ip := net.ParseIP(a)
			switch {
			case ip == nil:
				return fmt.Errorf("%s's probe %s address %q is not an address", node, what, a)
			case ip.To4() != nil && *v4 == "":
				*v4 = ip.String()
			case ip.To4() == nil && *v6 == "":
				*v6 = ip.String()
			}
		}
		if *v4 == "" {
			return fmt.Errorf("%s's probe has no IPv4 %s address in %v", node, what, addrs)
		}
		return nil
	}
	if err := assign(podIPs, &t.PodIP, &t.PodIP6, "pod"); err != nil {
		return Target{}, err
	}
	if err := assign(serviceIPs, &t.ServiceIP, &t.ServiceIP6, "service"); err != nil {
		return Target{}, err
	}
	if (t.PodIP6 == "") != (t.ServiceIP6 == "") {
		return Target{}, fmt.Errorf("%s's probe pod has addresses %v but its Service %v: the families disagree", node, podIPs, serviceIPs)
	}
	return t, nil
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

// HTTPSize posts TransferBytes from the probe pod to url's echo and counts
// the body that comes back, so the exchange crosses the tested pod network
// in full in both directions; only the count traverses serial or SSM.
// pipefail prevents a failed curl from passing via wc.
func (p *Pods) HTTPSize(ctx context.Context, node, url string) (int64, error) {
	cid, err := p.container(ctx, node)
	if err != nil {
		return 0, err
	}
	out, err := p.crictl(ctx, node, "exec", cid, "sh", "-ec", TransferCommand, "cldt-transfer", url, TransferFile)
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

// Lookup asks cluster DNS for name's addresses of one family from within
// the probe pod on node.
func (p *Pods) Lookup(ctx context.Context, node, name string, family Family) ([]string, error) {
	cid, err := p.container(ctx, node)
	if err != nil {
		return nil, err
	}
	qtype := "A"
	if family == IPv6 {
		qtype = "AAAA"
	}
	out, err := p.crictl(ctx, node, "exec", cid, "nslookup", "-type="+qtype, name)
	if err != nil {
		return nil, err
	}
	return NslookupAnswers(string(out)), nil
}

// NslookupAnswers are the addresses in nslookup's answer section: the
// "Address" lines after the first "Name" line. Those before it name the
// server that was asked.
func NslookupAnswers(out string) []string {
	var answers []string
	inAnswer := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Name:"):
			inAnswer = true
		case inAnswer && strings.HasPrefix(line, "Address:"):
			if ip := net.ParseIP(strings.TrimSpace(strings.TrimPrefix(line, "Address:"))); ip != nil {
				answers = append(answers, ip.String())
			}
		}
	}
	return answers
}

// UDPEcho runs the UDP prober inside the probe pod on node, carrying it
// in first if this container does not have it yet. Each attempt uses a
// fresh socket and the prober reports every one.
func (p *Pods) UDPEcho(ctx context.Context, node, destination string, payload, tries int, dontFragment bool) (UDPReport, error) {
	if len(p.UDPProbe) == 0 {
		return UDPReport{}, fmt.Errorf("no UDP prober was given to carry into the probe pods")
	}
	cid, err := p.container(ctx, node)
	if err != nil {
		return UDPReport{}, err
	}
	if err := p.carry(ctx, node, cid); err != nil {
		return UDPReport{}, err
	}
	// A failed echo exits nonzero and still prints its report.
	out, runErr := p.crictl(ctx, node, append([]string{"exec", cid}, UDPProbeArgv(destination, payload, tries, dontFragment)...)...)
	report, err := ParseUDPReport(out)
	if err != nil {
		if runErr != nil {
			return UDPReport{}, runErr
		}
		return UDPReport{}, err
	}
	return report, nil
}

// ParseUDPReport reads the prober's JSON report.
func ParseUDPReport(out []byte) (UDPReport, error) {
	var raw struct {
		OK       bool `json:"ok"`
		Attempts []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &raw); err != nil {
		return UDPReport{}, fmt.Errorf("reading the UDP prober's report %q: %w", truncate(string(out)), err)
	}
	report := UDPReport{OK: raw.OK, Attempts: len(raw.Attempts)}
	seen := map[string]bool{}
	for _, a := range raw.Attempts {
		if a.OK {
			report.Echoed++
		} else if a.Error != "" && !seen[a.Error] {
			seen[a.Error] = true
			report.Errors = append(report.Errors, a.Error)
		}
	}
	return report, nil
}

// carry puts the UDP prober into a probe container once.
func (p *Pods) carry(ctx context.Context, node, cid string) error {
	p.mu.Lock()
	done := p.carried[cid]
	p.mu.Unlock()
	if done {
		return nil
	}
	argv := []string{"crictl"}
	if p.CRIEndpoint != "" {
		argv = append(argv, "--runtime-endpoint", p.CRIEndpoint)
	}
	argv = append(argv, "exec", "-i", cid, "sh", "-ec", CarryCommand, "cldt-carry", UDPProbePath)
	if _, err := p.Rig.Node(node).Pipe(ctx, bytes.NewReader(p.UDPProbe), argv...); err != nil {
		return fmt.Errorf("carrying the UDP prober into %s's probe: %w", node, err)
	}
	p.mu.Lock()
	if p.carried == nil {
		p.carried = map[string]bool{}
	}
	p.carried[cid] = true
	p.mu.Unlock()
	return nil
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

var (
	_ Prober         = (*Pods)(nil)
	_ TransferProber = (*Pods)(nil)
	_ LookupProber   = (*Pods)(nil)
	_ UDPProber      = (*Pods)(nil)
)
