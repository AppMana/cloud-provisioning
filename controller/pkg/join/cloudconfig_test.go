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

// --join-ssh-authorized-keys says "on every new node", and the
// reconciler renders the value into every pattern. Only k0s ever read
// it: an operator who set the flag and ran kubeadm, k3s or RKE2 got
// no keys and no complaint, which is the worst shape a configuration
// bug can take, because the option looks supported everywhere.
//
// Also asserts the empty case, which is the default: a pattern that
// emits a bare "ssh_authorized_keys:" with nothing under it has
// written a null, and a real cloud-init reads that as a key list that
// is present and empty rather than absent.
func TestEveryPatternHonoursTheAuthorizedKeysFlag(t *testing.T) {
	patterns, err := filepath.Glob(filepath.Join("..", "..", "..", "join-patterns", "*.cloud-config.tmpl"))
	if err != nil || len(patterns) == 0 {
		t.Fatalf("no patterns found: %v", err)
	}

	for _, path := range patterns {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			withKeys := renderPatternWith(t, name, map[string]any{
				"sshAuthorizedKeys": []string{"ssh-ed25519 AAAAKEY operator@example"},
			})
			if !strings.Contains(withKeys, "ssh_authorized_keys:") {
				t.Error("the pattern ignores --join-ssh-authorized-keys, so the flag silently does nothing here")
			}
			if !strings.Contains(withKeys, "ssh-ed25519 AAAAKEY operator@example") {
				t.Error("the key never reaches the document")
			}

			none := renderPatternWith(t, name, map[string]any{"sshAuthorizedKeys": []string{}})
			if strings.Contains(none, "ssh_authorized_keys:") {
				t.Error("with no keys the pattern still emits ssh_authorized_keys, which is a null list rather than an absent one")
			}
		})
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
		"sshAuthorizedKeys":       []string{},
		"nodeAddress":             "",
		"providerID":              "",
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
		"sshAuthorizedKeys":       []string{},
		"nodeAddress":             "",
		"providerID":              "",
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
// renderPatternWith renders any pattern with a full set of values,
// overridden by the caller's. Patterns read different keys, so a test
// that applies to all of them cannot supply only the ones it cares
// about.
func renderPatternWith(t *testing.T, name string, override map[string]any) string {
	t.Helper()
	values := map[string]any{
		"peersFileJSON":           "{}",
		"wireguardAddress":        "10.100.0.2/24",
		"machineName":             "remote1",
		"interfaceName":           "cldt0",
		"wireguardListenPort":     "51820",
		"apiProxyPort":            7445,
		"apiEndpoint":             "10.10.0.10:6443",
		"joinEndpoint":            "10.10.0.10:6443",
		"joinToken":               "t.t",
		"caCertHash":              "sha256:x",
		"kubeletExtraArgs":        "",
		"sshAuthorizedKeys":       []string{},
		"joinServerURL":           "https://10.10.0.10:6443",
		"k3sVersion":              "v1.34.0+k3s1",
		"rke2Version":             "v1.34.0+rke2r1",
		"k0sVersion":              "v1.34.0+k0s.0",
		"microk8sRevision":        "9063",
		"microk8sJoinURL":         "10.10.0.10:25000/0123456789abcdef0123456789abcdef/0123456789ab",
		"dialerBinaryURLArm64":    "https://example.invalid/a",
		"dialerBinarySHA256Arm64": "a",
		"dialerBinaryURLAmd64":    "https://example.invalid/b",
		"dialerBinarySHA256Amd64": "b",
		"cniPluginsURLArm64":      "https://example.invalid/c",
		"cniPluginsSHA256Arm64":   "c",
		"cniPluginsURLAmd64":      "https://example.invalid/d",
		"cniPluginsSHA256Amd64":   "d",
		"nodeAddress":             "",
		"providerID":              "",
	}
	for k, v := range override {
		values[k] = v
	}
	return renderPattern(t, name, values)
}

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
		"sshAuthorizedKeys":       []string{},
		"nodeAddress":             "",
		"providerID":              "",
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

// A pattern that asks a cloud's instance-metadata service for the
// node's addresses must survive not being on that cloud.
//
// The service answers on a link-local address that nothing routes, so
// where it is absent the request does not fail — it hangs until the
// connection times out, and then, if the command was not allowed to
// fail, takes the rest of the bootstrap with it. Measured on a real
// first boot, in a lab with no metadata service:
//
//	curl: (28) Failed to connect to 169.254.169.254 port 80
//	      after 129587 ms: Connection timed out
//	cc_scripts_user.py[WARNING]: Failed to run module scripts_user
//
// The dialer was installed and the tunnel was up; "k0s install
// worker" and "k0s start" were in the same aborted block and never
// ran. The node joined nothing, and did it slowly.
//
// The addresses are an optimisation — a distribution that cannot get
// them falls back to resolving its own hostname, which is what it
// does everywhere this operator is not — so absence has to be cheap
// and survivable. Two of the patterns already bounded the wait and
// tolerated the failure; this makes it every pattern's contract
// rather than a habit two of them happened to have.
func TestEveryPatternSurvivesTheAbsenceOfAMetadataService(t *testing.T) {
	patterns, err := filepath.Glob(filepath.Join("..", "..", "..", "join-patterns", "*.cloud-config.tmpl"))
	if err != nil || len(patterns) == 0 {
		t.Fatalf("no patterns found: %v", err)
	}

	for _, path := range patterns {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			rendered := renderPatternWith(t, name, nil)
			for _, line := range strings.Split(rendered, "\n") {
				if !strings.Contains(line, "169.254.169.254") {
					continue
				}
				if !strings.Contains(line, "curl") {
					continue
				}
				if !hasConnectBound(line) {
					t.Errorf("this request has no bound on how long it waits, "+
						"so off that cloud it stalls the boot instead of failing:\n  %s",
						strings.TrimSpace(line))
				}
				if !strings.Contains(line, "|| true") {
					t.Errorf("this request is not allowed to fail, so its absence "+
						"aborts everything after it in the same block:\n  %s",
						strings.TrimSpace(line))
				}
			}
		})
	}
}

// hasConnectBound reports whether a curl is told how long it may wait.
// Either bound will do: -m caps the whole request, --connect-timeout
// caps the part that hangs when nothing answers.
func hasConnectBound(line string) bool {
	return strings.Contains(line, "-m ") || strings.Contains(line, "--max-time") ||
		strings.Contains(line, "--connect-timeout")
}

// When the infrastructure provider knows where the machine is, the
// node is told, and nothing asks a metadata service at all.
//
// A kubelet left to choose gets it wrong wherever there is more than
// one address to choose from. Measured: a machine with an
// out-of-band interface beside its real one registered the
// out-of-band address, the one every machine in that lab shares,
// while the mesh had already published the other. It joined, went
// Ready, and carried an identity nothing was looking for.
func TestAKnownAddressReachesTheNodeAndReplacesTheMetadataLookup(t *testing.T) {
	patterns, err := filepath.Glob(filepath.Join("..", "..", "..", "join-patterns", "*.cloud-config.tmpl"))
	if err != nil || len(patterns) == 0 {
		t.Fatalf("no patterns found: %v", err)
	}
	for _, path := range patterns {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			rendered := renderPatternWith(t, name, map[string]any{"nodeAddress": "203.0.113.10"})
			address := "203.0.113.10"
			if name == "microk8s-worker.cloud-config.tmpl" {
				// Native direct kubelet calls must use the allocated tunnel
				// address, not an unreachable cloud-private NIC address.
				address = "TUNNEL_NODE_ADDRESS="
			}
			if !strings.Contains(rendered, address) {
				t.Error("the provider knows this machine's address and the node is never told it, " +
					"so the kubelet chooses for itself and chooses wrong wherever it has more than one")
			}
			if !strings.Contains(rendered, "node-ip") {
				t.Error("the address is present but not as the flag the kubelet reads")
			}
			if strings.Contains(rendered, "169.254.169.254") {
				t.Error("a metadata service is still asked for an address already known, " +
					"which off that cloud is two seconds of waiting for an answer nobody needs")
			}
		})
	}
}

// With no address known, the metadata lookup is still there: a
// provider that creates an instance before it knows its address —
// which is most of them — has nowhere else to get it.
func TestAnUnknownAddressStillAsksTheInstance(t *testing.T) {
	rendered := renderPatternWith(t, "k0s-worker.cloud-config.tmpl", nil)
	if !strings.Contains(rendered, "169.254.169.254") {
		t.Error("a machine whose address nobody reported has no way left to learn it")
	}
}

// Every pattern tells the node the identity Cluster API binds it by.
//
// Cluster API matches a Machine to a Node on spec.providerID, and
// some distributions assign one themselves as the node registers —
// k3s writes k3s://<name>, RKE2 writes rke2://<name>. A node left to
// do that carries an identity the Machine does not have, so
// status.nodeRef is never set; the controller that publishes a
// remote's pod block then never finds a node to publish for, and the
// remote joins, goes Ready, and is unreachable from the site while
// every component reports healthy. Measured on a k3s row: the node
// said k3s://remote1 and the Machine said containernet://remote1.
//
// The ones that do not self-assign are told too. It is the same fact,
// it belongs to the node at registration rather than being patched in
// afterwards by something standing in for a cloud controller manager,
// and a pattern that only worked because its distribution happened to
// stay quiet is not a pattern that works.
func TestEveryPatternTellsTheNodeItsProviderIdentity(t *testing.T) {
	patterns, err := filepath.Glob(filepath.Join("..", "..", "..", "join-patterns", "*.cloud-config.tmpl"))
	if err != nil || len(patterns) == 0 {
		t.Fatalf("no patterns found: %v", err)
	}
	const id = "containernet://remote1"
	for _, path := range patterns {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			with := renderPatternWith(t, name, map[string]any{"providerID": id})
			if !strings.Contains(with, id) {
				t.Errorf("the node is never told its provider identity, so a distribution "+
					"that assigns its own wins and nothing binds the Machine to it:\n%s", name)
			}
			if !strings.Contains(with, "provider-id") {
				t.Error("the identity is present but not as the flag the kubelet reads")
			}

			// And with none assigned, nothing is claimed: the node
			// keeps whatever its distribution or cloud gives it.
			without := renderPatternWith(t, name, map[string]any{"providerID": ""})
			if strings.Contains(without, "provider-id=") {
				t.Error("an empty provider identity was rendered as a flag, " +
					"which sets the node's identity to nothing at all")
			}
		})
	}
}

// A pattern that writes a config file must not write the same key
// twice.
//
// RKE2 takes its kubelet arguments as a YAML list in a config file,
// and YAML has no notion of appending: a second kubelet-arg key later
// in the file replaces the first rather than adding to it. Writing
// the operator's kubeletExtraArgs under one key and the provider
// identity under another silently drops the role label and the taint
// this operator renders — the node joins, schedulable and unlabelled,
// and nothing says so.
func TestNoPatternWritesAConfigKeyTwice(t *testing.T) {
	patterns, err := filepath.Glob(filepath.Join("..", "..", "..", "join-patterns", "*.cloud-config.tmpl"))
	if err != nil || len(patterns) == 0 {
		t.Fatalf("no patterns found: %v", err)
	}
	for _, path := range patterns {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			// Everything a real render carries at once, which is when
			// two writers of one key collide.
			rendered := renderPatternWith(t, name, map[string]any{
				"providerID":       "containernet://remote1",
				"nodeAddress":      "203.0.113.10",
				"kubeletExtraArgs": "--node-labels=role=cloud-worker --register-with-taints=x:NoSchedule",
			})
			counts := map[string]int{}
			for _, line := range strings.Split(rendered, "\n") {
				trimmed := strings.TrimSpace(line)
				for _, key := range []string{"kubelet-arg:", "node-ip:", "node-name:"} {
					// Only the writes, not a mention inside a comment.
					if strings.Contains(trimmed, "'"+key+"'") || strings.Contains(trimmed, `"`+key) {
						counts[key]++
					}
				}
			}
			for key, n := range counts {
				if n > 1 {
					t.Errorf("%s is written %d times; the later write replaces the earlier "+
						"and whatever it carried is silently lost", key, n)
				}
			}
		})
	}
}
