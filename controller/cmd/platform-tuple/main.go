// Command platform-tuple renders k0s/Calico Windows BGP resources and evaluates
// platform tuple health and environment checks (e.g. "k0s + calico,bgp + aws").
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tuple"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func run(ctx context.Context, args []string, out, diagnostics io.Writer) error {
	fs := flag.NewFlagSet("platform-tuple", flag.ContinueOnError)
	fs.SetOutput(diagnostics)

	var winOpts tuple.WindowsBGPOptions
	fs.Func("pod-label", "explicit pod label KEY=VALUE (repeatable)", func(value string) error {
		key, label, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return fmt.Errorf("pod-label requires KEY=VALUE")
		}
		if winOpts.PodLabels == nil {
			winOpts.PodLabels = map[string]string{}
		}
		if _, exists := winOpts.PodLabels[key]; exists {
			return fmt.Errorf("repeated pod label %q", key)
		}
		winOpts.PodLabels[key] = label
		return nil
	})
	fs.Func("image-pull-secret", "image pull secret name (repeatable)", func(value string) error {
		winOpts.ImagePullSecrets = append(winOpts.ImagePullSecrets, value)
		return nil
	})
	fs.Func("exclude-native-cni", "exclude cloud-provisioning native CNI label value on calico-node-windows (repeatable)", func(value string) error {
		winOpts.ExcludeNativeCNIs = append(winOpts.ExcludeNativeCNIs, value)
		return nil
	})
	fs.StringVar(&winOpts.NodeImage, "node-image", "", "immutable Calico Windows node image reference (@sha256:...)")
	fs.StringVar(&winOpts.APIHost, "api-host", "", "Windows-reachable IPv4 Kubernetes API address")
	fs.StringVar(&winOpts.APIPort, "api-port", "6443", "Kubernetes API port (443 or 6443)")
	fs.StringVar(&winOpts.ServiceCIDR, "service-cidr", "", "IPv4 Kubernetes Service CIDR")
	fs.StringVar(&winOpts.DNSAddress, "dns-address", "", "cluster DNS IPv4 Service address")
	fs.StringVar(&winOpts.AutodetectionMethod, "autodetection-method", "", "explicit Calico IPv4 autodetection method")
	fs.StringVar(&winOpts.IPv6AutodetectionMethod, "ipv6-autodetection-method", "", "explicit Calico IPv6 autodetection method")
	fs.IntVar(&winOpts.MTU, "mtu", 0, "explicit pod/Felix MTU (1280-9000)")
	fs.BoolVar(&winOpts.ExcludeCloudWorkers, "exclude-cloud-workers", false, "exclude cloud-worker role from calico-node-windows")
	fs.BoolVar(&winOpts.Offline, "offline", false, "set ImagePullPolicy=Never")

	mode := fs.String("mode", "check", "check (evaluate platform tuple), fix (reconcile live Calico MTU and Windows BGP settings), render (Windows BGP manifest), k0s-network (k0s spec fragment), or validate-k0s-config")
	tupleExpr := fs.String("tuple", "k0s + calico,bgp + aws", "platform tuple expression (e.g. \"k0s + calico,bgp + aws\")")
	k0sConfigPath := fs.String("k0s-config", "", "optional k0s.yaml or k0sctl.yaml path to validate")
	valuesPath := fs.String("values", "", "optional Helm values.yaml path to inspect for cloud-provisioning configuration")
	var manifestPaths []string
	fs.Func("manifests", "optional Kubernetes manifest YAML file or directory to inspect (repeatable or comma-separated)", func(value string) error {
		manifestPaths = append(manifestPaths, value)
		return nil
	})
	format := fs.String("format", "json", "output format for check mode: json or text")
	kubeconfig := fs.String("kubeconfig", "", "optional kubeconfig path for live cluster checks or reconciliation")
	kubecontext := fs.String("context", "", "optional kubeconfig context override")
	dryRun := fs.Bool("dry-run", false, "report live Calico/Windows BGP changes without persisting them (--mode=fix)")
	tunnelMTU := fs.Int("tunnel-mtu", 1420, "WireGuard tunnel MTU budget")
	tunnelIPv4 := fs.String("tunnel-ipv4", "10.100.0.0/24", "WireGuard IPv4 tunnel range")
	tunnelIPv6 := fs.String("tunnel-ipv6", "fd00:10:100::/96", "WireGuard IPv6 tunnel range")
	requireDualStack := fs.Bool("require-dual-stack", false, "require IPv4 + IPv6 dual-stack pod networking")
	requireWindowsBGP := fs.Bool("require-windows-bgp", false, "require Windows BGP ConfigMap/DaemonSet/IPAMConfig even before a Windows node joins")
	strictBGPPeers := fs.Bool("strict-bgp-peers", false, "require BGPPeers to explicitly exclude cloud-worker even when cloudCNI=aws-vpc")
	podCIDR := fs.String("pod-cidr", "", "optional IPv4 pod CIDR for k0s-network mode")
	ipv6PodCIDR := fs.String("ipv6-pod-cidr", "", "optional IPv6 pod CIDR for k0s-network mode")
	ipv6ServiceCIDR := fs.String("ipv6-service-cidr", "", "optional IPv6 service CIDR for k0s-network mode")
	timeout := fs.Duration("timeout", 30*time.Second, "API request timeout")

	isVerb := func(v string) bool {
		switch v {
		case "check", "fix", "render", "k0s-network", "validate-k0s-config":
			return true
		default:
			return false
		}
	}
	var positionalVerb string
	if len(args) > 0 && isVerb(args[0]) {
		positionalVerb = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 && positionalVerb == "" && isVerb(fs.Arg(0)) {
		positionalVerb = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if positionalVerb != "" {
		*mode = positionalVerb
	}

	var k0sBytes []byte
	if *k0sConfigPath != "" {
		raw, err := os.ReadFile(*k0sConfigPath)
		if err != nil {
			return err
		}
		k0sBytes = raw
	}
	var valuesBytes []byte
	if *valuesPath != "" {
		raw, err := os.ReadFile(*valuesPath)
		if err != nil {
			return err
		}
		valuesBytes = raw
	}
	manifestsBytes, err := tuple.LoadManifestBytes(manifestPaths)
	if err != nil {
		return err
	}

	newKubeClient := func() (client.Client, error) {
		loading := clientcmd.NewDefaultClientConfigLoadingRules()
		if *kubeconfig != "" {
			loading = &clientcmd.ClientConfigLoadingRules{ExplicitPath: *kubeconfig}
		}
		cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, &clientcmd.ConfigOverrides{CurrentContext: *kubecontext}).ClientConfig()
		if err != nil {
			return nil, err
		}
		cfg.Timeout = *timeout
		sc := runtime.NewScheme()
		_ = clientgoscheme.AddToScheme(sc)
		for _, kind := range []string{"IPPool", "BlockAffinity", "IPAMBlock", "BGPPeer", "BGPConfiguration", "IPAMConfig"} {
			gvk := schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: kind}
			sc.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
			sc.AddKnownTypeWithName(gvk.GroupVersion().WithKind(kind+"List"), &unstructured.UnstructuredList{})
		}
		return client.New(cfg, client.Options{Scheme: sc})
	}

	switch *mode {
	case "validate-k0s-config":
		if len(k0sBytes) == 0 {
			return fmt.Errorf("validate-k0s-config requires --k0s-config")
		}
		if err := tuple.ValidateK0sCalicoConfig(k0sBytes); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]string{"status": "ok", "config": *k0sConfigPath})

	case "k0s-network":
		spec, err := tuple.K0sCalicoNetwork(tuple.K0sNetworkOptions{
			MTU:                     winOpts.MTU,
			AutodetectionMethod:     winOpts.AutodetectionMethod,
			IPv6AutodetectionMethod: winOpts.IPv6AutodetectionMethod,
			PodCIDR:                 *podCIDR,
			ServiceCIDR:             winOpts.ServiceCIDR,
			IPv6PodCIDR:             *ipv6PodCIDR,
			IPv6ServiceCIDR:         *ipv6ServiceCIDR,
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(spec)

	case "render":
		if len(k0sBytes) > 0 {
			if err := tuple.ValidateK0sCalicoConfig(k0sBytes); err != nil {
				return err
			}
		}
		plan, err := tuple.NewPlan(winOpts)
		if err != nil {
			return err
		}
		if err := json.NewEncoder(diagnostics).Encode(map[string]string{
			"mode":           "render",
			"manifestSHA256": plan.SHA256(),
		}); err != nil {
			return err
		}
		_, err = out.Write(plan.Manifest())
		return err

	case "fix":
		cl, err := newKubeClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		var writer client.Client = cl
		if *dryRun {
			writer = nil
		}
		report, err := tuple.ReconcileCalicoSite(ctx, writer, cl, winOpts.MTU)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)

	case "check":
		var reader client.Reader
		if *kubeconfig != "" {
			cl, err := newKubeClient()
			if err != nil {
				return err
			}
			reader = cl
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		results, err := tuple.Evaluate(ctx, reader, tuple.CheckOptions{
			Tuple:              *tupleExpr,
			K0sConfig:          k0sBytes,
			ValuesYAML:         valuesBytes,
			ManifestsYAML:      manifestsBytes,
			TunnelMTU:          *tunnelMTU,
			TunnelIPv4:         *tunnelIPv4,
			TunnelIPv6:         *tunnelIPv6,
			RequireDualStack:   *requireDualStack,
			RequireWindowsBGP:  *requireWindowsBGP,
			StrictBGPPeerScope: *strictBGPPeers,
		})
		if err != nil {
			return err
		}
		report := tuple.FormatReport(results)
		if strings.EqualFold(strings.TrimSpace(*format), "text") {
			if _, err := io.WriteString(out, report); err != nil {
				return err
			}
		} else {
			if diagnostics != nil {
				_, _ = io.WriteString(diagnostics, report)
			}
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			if err := enc.Encode(results); err != nil {
				return err
			}
		}
		if !tuple.Passed(results) {
			return fmt.Errorf("platform tuple %q failed one or more health/environment checks", *tupleExpr)
		}
		return nil

	default:
		return fmt.Errorf("unknown --mode %q (expected check, fix, render, k0s-network, or validate-k0s-config)", *mode)
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
