package join

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/render"
)

var templateActions = regexp.MustCompile(`{{.*?}}`)

// The dialer's systemd unit is written by a cloud-init template, as a
// shell-style continued command. A stray backslash in that continuation
// does not fail to render and does not fail to boot: systemd passes the
// backslash through as its own argument, Go's flag package stops
// parsing at the first argument that is not a flag, and every flag
// after it is silently discarded. The unit runs, the tunnel comes up,
// and the node quietly uses defaults for whatever the template thought
// it was setting.
//
// That shipped: both templates ended --listen-port with "\ \ \", so
// --keepalive-seconds, --mtu and --poll-interval never reached the
// dialer on any bootstrapped node.
func TestUnitExecStartPassesOnlyFlags(t *testing.T) {
	// Every template, the shared blocks included: the units live in
	// _shared.tmpl now, and a sweep that skipped it would lint
	// nothing.
	patterns, err := filepath.Glob(filepath.Join("..", "..", "..", "join-patterns", "*.tmpl"))
	if err != nil || len(patterns) == 0 {
		t.Fatalf("no join patterns found: %v", err)
	}
	for _, path := range patterns {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, cmd := range execStartCommands(string(raw)) {
			// A template action renders to one word but contains
			// spaces, so it has to be collapsed before the command is
			// split the way systemd splits it.
			fields := strings.Fields(templateActions.ReplaceAllString(cmd, "VALUE"))
			for _, arg := range fields[1:] {
				if !strings.HasPrefix(arg, "--") {
					t.Errorf("%s: ExecStart argument %q is not a flag, so Go stops parsing here and every later flag is dropped:\n  %s",
						filepath.Base(path), arg, cmd)
				}
			}
		}
	}
}

// execStartCommands returns each ExecStart= value with its line
// continuations joined, as systemd would assemble them. Template
// control lines ({{- if }}, {{- else }}, {{- end }}) render to
// nothing, so the joiner skips them the way the renderer erases them;
// what remains is what systemd sees in at least one rendering.
var controlLine = regexp.MustCompile(`^\{\{-?\s*(if|else|end)\b`)

func execStartCommands(tmpl string) []string {
	var cmds []string
	lines := strings.Split(tmpl, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		cmd := strings.TrimPrefix(line, "ExecStart=")
		for strings.HasSuffix(strings.TrimSpace(cmd), `\`) && i+1 < len(lines) {
			i++
			next := strings.TrimSpace(lines[i])
			if controlLine.MatchString(next) {
				continue
			}
			cmd = strings.TrimSuffix(strings.TrimSpace(cmd), `\`)
			cmd += " " + next
		}
		cmds = append(cmds, cmd)
	}
	return cmds
}

// The bootstrap unit is deliberately never disabled, so that a node
// whose DaemonSet cannot schedule stays reachable. That makes the
// machine-name file load-bearing: it is how the DaemonSet's shared pod
// spec finds this machine's own adoption Secret, which is the only
// source of a peer list that is not frozen at render time.
func TestPatternsWriteTheMachineNameForAdoption(t *testing.T) {
	// The file itself is written by the shared wg-dialer-files block,
	// so each pattern must invoke it (or write the path directly), and
	// the shared block must actually contain the path.
	shared, err := os.ReadFile(filepath.Join("..", "..", "..", "join-patterns", "_shared.tmpl"))
	if err != nil {
		t.Fatalf("reading _shared.tmpl: %v", err)
	}
	if !strings.Contains(string(shared), "/etc/wg-dialer/machine-name") {
		t.Fatal("_shared.tmpl's wg-dialer-files block writes no machine-name file")
	}
	patterns, _ := filepath.Glob(filepath.Join("..", "..", "..", "join-patterns", "*.cloud-config.tmpl"))
	for _, path := range patterns {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if !strings.Contains(string(raw), `template "wg-dialer-files"`) &&
			!strings.Contains(string(raw), "/etc/wg-dialer/machine-name") {
			t.Errorf("%s writes no machine-name file, so the DaemonSet cannot resolve this machine's adoption Secret", filepath.Base(path))
		}
	}
}

// The kubeadm pattern joins through the node's own loopback balancer
// and gates on it: one probe proves the tunnel, the balancer, and a
// live control plane. Joining a specific control plane's address
// instead pins kubelet to that member forever (kubeadm writes
// kubelet.conf from the join endpoint), and its death then strands
// the node with quorum intact.
func TestKubeadmJoinsThroughTheLoopbackBalancer(t *testing.T) {
	rendered := renderKubeadmPattern(t, 7445)
	if !strings.Contains(rendered, "kubeadm join 127.0.0.1:7445") {
		t.Error("the join does not dial the loopback balancer, so kubelet is pinned to one control plane")
	}
	if !strings.Contains(rendered, "https://127.0.0.1:7445/livez") {
		t.Error("the gate does not probe the loopback balancer, so a join can start before the balancer serves")
	}
	// The balancer is its own unit with its own lifecycle: kubelet's
	// API path must not share the dialer's restarts, crashes, or
	// hourly re-exec. The dialer unit therefore carries no balancer
	// flag at all: exactly one ExecStart names the port, and it is the
	// proxy's, in proxy-only mode.
	if !strings.Contains(rendered, "wg-apiproxy.service") {
		t.Error("no wg-apiproxy unit: the balancer would share the dialer's lifecycle, which is the failure envoy exists to avoid")
	}
	if !strings.Contains(rendered, "--api-proxy-only") {
		t.Error("the balancer unit does not run in proxy-only mode")
	}
	if n := strings.Count(rendered, "--api-proxy-port="); n != 1 {
		t.Errorf("--api-proxy-port appears %d times; exactly one unit (the proxy's) may carry it", n)
	}
	if !strings.Contains(rendered, "systemctl enable --now wg-apiproxy.service") {
		t.Error("the balancer unit is never enabled")
	}
}

// The balancer is per-distribution, not a constant of this operator:
// k3s and RKE2 agents carry their own client-side balancer, Talos has
// KubePrism, k0s has nllb, and only kubeadm-family clusters have
// nothing. Port zero is how a setup that balances for itself says so,
// and the pattern must then join the endpoint directly, exactly as it
// did before the balancer existed.
func TestKubeadmWithoutTheBalancerJoinsTheEndpointDirectly(t *testing.T) {
	rendered := renderKubeadmPattern(t, 0)
	if strings.Contains(rendered, "--api-proxy-port") {
		t.Error("port zero still passes --api-proxy-port, so a unit serves a balancer nobody asked for")
	}
	if strings.Contains(rendered, "wg-apiproxy.service") {
		t.Error("port zero still renders the balancer unit")
	}
	if !strings.Contains(rendered, "kubeadm join 10.101.0.1:6443") {
		t.Error("port zero does not join the endpoint directly")
	}
	if !strings.Contains(rendered, "https://10.101.0.1:6443/livez") {
		t.Error("port zero does not gate on the endpoint directly")
	}
	if strings.Contains(rendered, "127.0.0.1:0") {
		t.Error("a literal port zero leaked into the render")
	}
}

// kubeadm's kubelet drop-in sources /etc/default/kubelet on
// Debian-family systems and /etc/sysconfig/kubelet on RPM-family
// (RHEL, Fedora, Amazon Linux). A pattern that writes only the Debian
// path silently drops the role label and the internet-facing taint on
// half the distributions it claims to support: the node joins, looks
// healthy, and is missing exactly the properties that make it a cloud
// worker.
func TestKubeletArgsReachBothFamilies(t *testing.T) {
	rendered := renderKubeadmPattern(t, 7445)
	for _, path := range []string{"/etc/default/kubelet", "/etc/sysconfig/kubelet"} {
		if !strings.Contains(rendered, path) {
			t.Errorf("the kubeadm pattern does not write %s, so kubelet args are lost on that family", path)
		}
	}
}

// The k3s agent carries its own client-side balancer across every
// server it learns from the supervisor, so its pattern must show no
// trace of the operator's balancer: no --api-proxy-port in the unit
// and no loopback join. It gates on and joins the rendered endpoint,
// with the token in a root-only file rather than on a command line.
func TestK3sJoinsTheEndpointAndCarriesNoSecondBalancer(t *testing.T) {
	rendered := renderPattern(t, "k3s-worker.cloud-config.tmpl", map[string]any{
		"peersFileJSON":           "{}",
		"machineName":             "remote1",
		"interfaceName":           "cldt0",
		"wireguardListenPort":     "51820",
		"apiEndpoint":             "10.101.0.1:6443",
		"joinServerURL":           "https://10.101.0.1:6443",
		"joinToken":               "K10aaaa::id.secret",
		"k3sVersion":              "v1.33.3+k3s1",
		"apiProxyPort":            0,
		"kubeletExtraArgs":        "--node-labels=x=y",
		"dialerBinaryURLArm64":    "https://example.com/a",
		"dialerBinarySHA256Arm64": "a",
		"dialerBinaryURLAmd64":    "https://example.com/b",
		"dialerBinarySHA256Amd64": "b",
	})
	if strings.Contains(rendered, "--api-proxy-port") {
		t.Error("the k3s pattern passes --api-proxy-port, stacking a second balancer on the agent's own")
	}
	if !strings.Contains(rendered, "https://10.101.0.1:6443/livez") {
		t.Error("the gate does not probe the rendered endpoint")
	}
	if !strings.Contains(rendered, "--server 'https://10.101.0.1:6443'") {
		t.Error("the agent is not pointed at the supervisor")
	}
	if !strings.Contains(rendered, "INSTALL_K3S_VERSION='v1.33.3+k3s1'") {
		t.Error("the install is not pinned to the cluster's own version")
	}
	if !strings.Contains(rendered, "--token-file /etc/rancher/k3s/join-token") {
		t.Error("the token does not travel by file")
	}
	if strings.Contains(rendered, "K3S_TOKEN=") {
		t.Error("the token leaked onto a command line")
	}
}

// RKE2 differs from k3s in exactly the ways its provider says it
// does: the join dials the supervisor on 9345, everything travels
// through config.yaml plus a token file, and again no second
// balancer.
func TestRKE2JoinsTheSupervisorAndCarriesNoSecondBalancer(t *testing.T) {
	rendered := renderPattern(t, "rke2-worker.cloud-config.tmpl", map[string]any{
		"peersFileJSON":           "{}",
		"machineName":             "remote1",
		"interfaceName":           "cldt0",
		"wireguardListenPort":     "51820",
		"apiEndpoint":             "10.101.0.1:6443",
		"joinServerURL":           "https://10.101.0.1:9345",
		"joinToken":               "K10aaaa::id.secret",
		"rke2Version":             "v1.33.4+rke2r1",
		"apiProxyPort":            0,
		"kubeletExtraArgs":        "--node-labels=x=y",
		"dialerBinaryURLArm64":    "https://example.com/a",
		"dialerBinarySHA256Arm64": "a",
		"dialerBinaryURLAmd64":    "https://example.com/b",
		"dialerBinarySHA256Amd64": "b",
	})
	if strings.Contains(rendered, "--api-proxy-port") {
		t.Error("the rke2 pattern passes --api-proxy-port, stacking a second balancer on the agent's own")
	}
	if !strings.Contains(rendered, "server: https://10.101.0.1:9345") {
		t.Error("the agent's config does not point at the supervisor")
	}
	if !strings.Contains(rendered, "https://10.101.0.1:9345/ping") {
		t.Error("the gate does not probe the supervisor the join will dial")
	}
	if !strings.Contains(rendered, "INSTALL_RKE2_VERSION='v1.33.4+rke2r1'") {
		t.Error("the install is not pinned to the cluster's own version")
	}
	if !strings.Contains(rendered, "token-file: /etc/rancher/rke2/join-token") {
		t.Error("the token does not travel by file")
	}
	if strings.Contains(rendered, "RKE2_TOKEN=") {
		t.Error("the token leaked onto a command line")
	}
}

// renderPattern renders through pkg/render, the reconciler's own
// path, so the shared blocks parse here exactly as they do in
// production.
func renderPattern(t *testing.T, name string, values map[string]any) string {
	t.Helper()
	rendered, err := render.Pattern(filepath.Join("..", "..", "..", "join-patterns", name), values)
	if err != nil {
		t.Fatalf("rendering %s: %v", name, err)
	}
	return rendered
}

func renderKubeadmPattern(t *testing.T, proxyPort int) string {
	t.Helper()
	return renderPattern(t, "kubeadm-worker.cloud-config.tmpl", map[string]any{
		"peersFileJSON":           "{}",
		"machineName":             "remote1",
		"interfaceName":           "cldt0",
		"wireguardListenPort":     "51820",
		"apiProxyPort":            proxyPort,
		"apiEndpoint":             "10.101.0.1:6443",
		"joinEndpoint":            "10.101.0.1:6443",
		"joinToken":               "t.t",
		"caCertHash":              "sha256:x",
		"kubeletExtraArgs":        "",
		"dialerBinaryURLArm64":    "https://example.com/a",
		"dialerBinarySHA256Arm64": "a",
		"dialerBinaryURLAmd64":    "https://example.com/b",
		"dialerBinarySHA256Amd64": "b",
	})
}
