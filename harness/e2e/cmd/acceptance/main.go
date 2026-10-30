// Command acceptance qualifies a real cluster for the product from an
// explicit kubeconfig, with no bastion and no node access.
//
//	acceptance -kubeconfig FILE preconditions
//	acceptance -kubeconfig FILE matrix
//
// preconditions only reads: its client refuses every request that is not
// a GET. matrix creates probe pods and Services in one labelled namespace
// of its own, runs the reachability matrix through pod exec, and deletes
// that namespace on the way out, interrupted or not.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/acceptance"
	"github.com/appmana/cloud-provisioning/harness/e2e/check"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	return runWith(args, preconditions, runMatrix)
}

func runWith(args []string,
	preflight func(context.Context, *rest.Config, acceptance.Options) ([]acceptance.Check, error),
	probe func(context.Context, *rest.Config, string, int, time.Duration, string, string, string, func(int, *check.Matrix)) (*check.Matrix, error),
) error {
	flags := flag.NewFlagSet("acceptance", flag.ContinueOnError)
	kubeconfig := flags.String("kubeconfig", "", "REQUIRED kubeconfig file; $KUBECONFIG and ~/.kube/config are never read")
	kubeContext := flags.String("context", "", "context in that file (default: its current context)")
	tunnelMTU := flags.Int("tunnel-mtu", 1420, "tunnel device MTU: the endpoints' underlay MTU less 80")
	tunnel4 := flags.String("tunnel-ipv4", "10.100.0.0/24", "tunnel IPv4 range (chart tunnel subnet)")
	tunnel6 := flags.String("tunnel-ipv6", "fd00:10:100::/96", "tunnel IPv6 prefix (chart tunnel.ipv6Prefix)")
	tupleExpr := flags.String("tuple", "", "optional platform tuple to qualify (e.g. \"k0s + calico,bgp + aws\")")
	k0sConfigPath := flags.String("k0s-config", "", "optional k0s.yaml or k0sctl.yaml path to validate")
	out := flags.String("out", "", "write the JSON result here (must not exist)")
	nodes := flags.String("nodes", "", "matrix: comma-separated nodes (default: every Ready Linux node)")
	tries := flags.Int("udp-tries", 10, "matrix: datagrams per UDP size")
	window := flags.Duration("window", 10*time.Minute, "matrix: convergence window")
	udpFile := flags.String("udp-probe-file", "", "optional prebuilt Linux UDP prober for running without a Go toolchain")
	udpSHA := flags.String("udp-probe-sha256", "", "required SHA256 of --udp-probe-file")
	moduleDir := flags.String("module-dir", ".", "matrix: harness module directory the UDP prober is built from")
	timeout := flags.Duration("timeout", 45*time.Minute, "deadline for the whole command")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || (flags.Arg(0) != "preconditions" && flags.Arg(0) != "matrix") {
		return fmt.Errorf("usage: acceptance -kubeconfig FILE [flags] preconditions|matrix")
	}
	if *out != "" {
		if _, err := os.Stat(*out); err == nil {
			return fmt.Errorf("%s exists; use a new evidence path", *out)
		}
	}
	var k0sBytes []byte
	if *k0sConfigPath != "" {
		raw, err := os.ReadFile(*k0sConfigPath)
		if err != nil {
			return err
		}
		k0sBytes = raw
	}
	config, err := acceptance.Config(*kubeconfig, *kubeContext)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	opts := acceptance.Options{TunnelMTU: *tunnelMTU, TunnelIPv4: *tunnel4, TunnelIPv6: *tunnel6, Tuple: *tupleExpr, K0sConfig: k0sBytes}
	result := map[string]any{
		"observedAt": time.Now().UTC(),
		"server":     config.Host,
		"mode":       flags.Arg(0),
		"options":    opts,
	}
	// Preconditions always run, read-only, before anything is created.
	checks, err := preflight(ctx, config, opts)
	if err != nil {
		return err
	}
	result["preconditions"] = checks
	result["preconditionsPassed"] = acceptance.Passed(checks)
	for _, c := range checks {
		verdict := "PASS"
		switch {
		case !c.OK && c.Required:
			verdict = "FAIL"
		case !c.Required:
			verdict = "INFO"
		}
		fmt.Printf("  %-4s  %-20s %s\n", verdict, c.Name, c.Detail)
	}

	ok := acceptance.Passed(checks)
	if flags.Arg(0) == "matrix" && !ok {
		result["matrixSkipped"] = "required preconditions failed; no probes created"
	}
	if flags.Arg(0) == "matrix" && ok {
		passes := []map[string]any{}
		failedChecks := 0
		allPassed := true
		matrix, err := probe(ctx, config, *nodes, *tries, *window, *moduleDir, *udpFile, *udpSHA, func(attempt int, m *check.Matrix) {
			passes = append(passes, map[string]any{"attempt": attempt, "matrix": m.Details()})
			failedChecks += m.Failed()
			allPassed = allPassed && m.OK()
		})
		result["passes"] = passes
		result["allAttemptsPassed"] = len(passes) > 0 && allPassed
		result["failedChecksAcrossPasses"] = failedChecks
		if matrix == nil && err == nil {
			err = fmt.Errorf("probe returned no matrix evidence")
		}
		if len(passes) > 0 {
			fmt.Printf("  observed %d passes and %d failed checks across all attempts\n", len(passes), failedChecks)
		}
		if matrix != nil {
			fmt.Println(matrix.Report())
			result["matrix"] = matrix.Details()
			ok = ok && matrix.OK() && len(passes) > 0 && allPassed
		}
		if err != nil {
			result["matrixError"] = err.Error()
			ok = false
		}
	}
	result["ok"] = ok
	if *out != "" {
		raw, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(raw, '\n'), 0o600); err != nil {
			return err
		}
	}
	if !ok {
		return fmt.Errorf("the cluster does not meet the product's requirements")
	}
	return nil
}

func preconditions(ctx context.Context, config *rest.Config, opts acceptance.Options) ([]acceptance.Check, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	reader, err := client.New(acceptance.ReadOnly(config), client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	return acceptance.Preconditions(ctx, reader, opts)
}

func runMatrix(ctx context.Context, config *rest.Config, nodeList string, tries int, window time.Duration, moduleDir, udpFile, udpSHA string, observe func(int, *check.Matrix)) (*check.Matrix, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	var nodes []string
	for _, n := range strings.Split(nodeList, ",") {
		if n = strings.TrimSpace(n); n != "" {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		if nodes, err = acceptance.LinuxNodes(ctx, clientset); err != nil {
			return nil, err
		}
	}
	if len(nodes) < 2 {
		return nil, fmt.Errorf("a matrix needs at least two nodes, have %v", nodes)
	}
	// The prober is built in the module's private state directory.
	state := filepath.Join(moduleDir, ".state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(state, "acceptance-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	var udpProbe []byte
	var udpProbes map[string][]byte
	if udpFile == "" && udpSHA == "" {
		udpProbes, err = check.BuildUDPProbes(ctx, moduleDir, filepath.Join(work, "build"))
	} else {
		udpProbe, err = loadUDPProbe(ctx, moduleDir, filepath.Join(work, "build"), udpFile, udpSHA)
	}
	if err != nil {
		return nil, err
	}
	pods := &acceptance.Pods{
		Config: config, Client: clientset,
		Namespace:  fmt.Sprintf("cloud-provisioning-acceptance-%d", time.Now().Unix()),
		PullPolicy: corev1.PullIfNotPresent,
		UDPProbe:   udpProbe,
		UDPProbes:  udpProbes,
	}
	defer func() {
		// Its own clock: the run's may already be spent or cancelled.
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if err := pods.Stop(cleanup); err != nil {
			fmt.Fprintf(os.Stderr, "removing probe namespace %s: %v\n", pods.Namespace, err)
		} else {
			fmt.Printf("  removed probe namespace %s\n", pods.Namespace)
		}
	}()
	fmt.Printf("  probing %v from namespace %s\n", nodes, pods.Namespace)
	targets, err := pods.Start(ctx, nodes, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	return check.Converge(ctx, pods, targets, check.Options{
		Port: check.Port, ExternalURL: check.ExternalURL, UDP: &check.UDPOptions{Tries: tries}, ObservePass: observe,
	}, window, 15*time.Second), nil
}

func loadUDPProbe(ctx context.Context, moduleDir, work, path, expected string) ([]byte, error) {
	if path == "" && expected == "" {
		return check.BuildUDPProbe(ctx, moduleDir, work)
	}
	if path == "" || len(expected) != 64 {
		return nil, fmt.Errorf("prebuilt UDP prober requires a file and SHA256")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != expected {
		return nil, fmt.Errorf("UDP prober SHA256 mismatch")
	}
	return data, nil
}
