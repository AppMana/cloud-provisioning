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
	flags := flag.NewFlagSet("acceptance", flag.ContinueOnError)
	kubeconfig := flags.String("kubeconfig", "", "REQUIRED kubeconfig file; $KUBECONFIG and ~/.kube/config are never read")
	kubeContext := flags.String("context", "", "context in that file (default: its current context)")
	tunnelMTU := flags.Int("tunnel-mtu", 1420, "tunnel device MTU: the endpoints' underlay MTU less 80")
	tunnel4 := flags.String("tunnel-ipv4", "10.100.0.0/24", "tunnel IPv4 range (chart tunnel subnet)")
	tunnel6 := flags.String("tunnel-ipv6", "fd00:10:100::/96", "tunnel IPv6 prefix (chart tunnel.ipv6Prefix)")
	out := flags.String("out", "", "write the JSON result here (must not exist)")
	nodes := flags.String("nodes", "", "matrix: comma-separated nodes (default: every Ready Linux node)")
	tries := flags.Int("udp-tries", 10, "matrix: datagrams per UDP size")
	window := flags.Duration("window", 10*time.Minute, "matrix: convergence window")
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
	config, err := acceptance.Config(*kubeconfig, *kubeContext)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	opts := acceptance.Options{TunnelMTU: *tunnelMTU, TunnelIPv4: *tunnel4, TunnelIPv6: *tunnel6}
	result := map[string]any{
		"observedAt": time.Now().UTC(),
		"server":     config.Host,
		"mode":       flags.Arg(0),
		"options":    opts,
	}
	// Preconditions always run, read-only, before anything is created.
	checks, err := preconditions(ctx, config, opts)
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
	if flags.Arg(0) == "matrix" {
		matrix, err := runMatrix(ctx, config, *nodes, *tries, *window, *moduleDir)
		if matrix != nil {
			fmt.Println(matrix.Report())
			result["matrix"] = matrix.Details()
			ok = ok && matrix.OK()
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

func runMatrix(ctx context.Context, config *rest.Config, nodeList string, tries int, window time.Duration, moduleDir string) (*check.Matrix, error) {
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
	work, err := os.MkdirTemp("", "cloud-provisioning-acceptance-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	udpProbe, err := check.BuildUDPProbe(ctx, moduleDir, filepath.Join(work, "build"))
	if err != nil {
		return nil, err
	}
	pods := &acceptance.Pods{
		Config: config, Client: clientset,
		Namespace:  fmt.Sprintf("cloud-provisioning-acceptance-%d", time.Now().Unix()),
		PullPolicy: corev1.PullIfNotPresent,
		UDPProbe:   udpProbe,
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
		Port: check.Port, ExternalURL: check.ExternalURL, UDP: &check.UDPOptions{Tries: tries},
	}, window, 15*time.Second), nil
}
