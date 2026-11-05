// Package cli implements the unified CLI entrypoint for platform-tuple and
// endpoint-controller subcommands (check, fix, iam, iam-render, iam-apply,
// iam-cleanup, render, k0s-network, validate-k0s-config).
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	awsiam "github.com/appmana/cloud-provisioning/controller/pkg/platform/aws/iam"
	"github.com/appmana/cloud-provisioning/controller/pkg/tuple"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IsCommand reports whether arg is a known platform-tuple / endpoint-controller
// CLI subcommand verb.
func IsCommand(arg string) bool {
	switch arg {
	case "check", "fix", "iam", "iam-render", "iam-apply", "iam-cleanup", "render", "k0s-network", "validate-k0s-config":
		return true
	default:
		return false
	}
}

// Main runs the CLI with interrupt handling and returns an exit code (0, 1, or 2).
func Main(args []string, out, diagnostics io.Writer) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	// Default output format for 'endpoint-controller check' is text when not overridden,
	// whereas 'platform-tuple' defaults to json; Run handles both via DefaultFormat.
	if err := Run(ctx, args, out, diagnostics); err != nil {
		if diagnostics != nil {
			fmt.Fprintln(diagnostics, err)
		}
		return 1
	}
	return 0
}

// Run executes a platform-tuple / endpoint-controller subcommand.
func Run(ctx context.Context, args []string, out, diagnostics io.Writer) error {
	// Normalize "iam <render|apply|cleanup>" into "iam-<verb>".
	if len(args) > 0 && args[0] == "iam" {
		verb := "render"
		restArgs := args[1:]
		if len(restArgs) > 0 && (restArgs[0] == "render" || restArgs[0] == "apply" || restArgs[0] == "cleanup") {
			verb = restArgs[0]
			restArgs = restArgs[1:]
		}
		args = append([]string{"iam-" + verb}, restArgs...)
	}

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

	mode := fs.String("mode", "check", "check, fix, iam-render, iam-apply, iam-cleanup, render, k0s-network, or validate-k0s-config")
	tupleExpr := new(string)
	*tupleExpr = "k0s + calico,bgp + aws"
	fs.StringVar(tupleExpr, "target-config", *tupleExpr, "target config expression (e.g. \"aws,aws-vpc;gcp,site\" or \"k0s + calico,bgp + aws\")")
	fs.StringVar(tupleExpr, "tuple", *tupleExpr, "platform tuple / target config expression (alias for --target-config)")
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
	dryRun := fs.Bool("dry-run", false, "report live Calico/Windows BGP/IAM changes without persisting them")
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

	var iamCfg tuple.IAMConfig
	// Register both --aws-* (platform-tuple convention) and unprefixed (endpoint-controller iam convention) flags.
	bindString := func(p *string, def, usage string, names ...string) {
		*p = def
		for _, n := range names {
			fs.StringVar(p, n, def, usage)
		}
	}
	bindBool := func(p *bool, def bool, usage string, names ...string) {
		*p = def
		for _, n := range names {
			fs.BoolVar(p, n, def, usage)
		}
	}
	bindFunc := func(usage string, fn func(string) error, names ...string) {
		for _, n := range names {
			fs.Func(n, usage, fn)
		}
	}

	bindString(&iamCfg.Scope, "account", "IAM scope: 'account' (shared clusterawsadm roles + AWS-Managed Policies across all clusters) or 'cluster' (isolated per-cluster roles)", "aws-iam-scope", "scope")
	bindString(&iamCfg.BundleID, "", "optional explicit IAM bundle ownership tag ID (defaults to 'account' or --aws-role-prefix)", "aws-iam-bundle-id", "bundle-id")
	bindFunc("legacy IAM bundle ID whose obsolete Customer Managed Policies may be detached and pruned during iam-apply (repeatable)", func(v string) error {
		iamCfg.MigrateFromBundles = append(iamCfg.MigrateFromBundles, v)
		return nil
	}, "aws-migrate-from-bundle", "migrate-from-bundle")
	bindString(&iamCfg.AccountID, "", "AWS 12-digit account ID (auto-detected via STS in iam-apply when omitted)", "aws-account-id", "account-id")
	bindString(&iamCfg.Region, "", "AWS region for IAM policy scoping (auto-discovered from AWSCluster when --kubeconfig is set)", "aws-region", "region")
	bindString(&iamCfg.ClusterName, "", "CAPI cluster name for IAM tag scoping (auto-discovered from AWSCluster when --kubeconfig is set)", "aws-cluster", "cluster")
	bindString(&iamCfg.RolePrefix, "", "optional IAM role/policy name prefix (defaults to --aws-cluster)", "aws-role-prefix", "role-prefix")
	bindString(&iamCfg.OwnerTag, "", "optional resource ownership tag key (defaults to CAPA cluster tag)", "aws-owner-tag", "owner-tag")
	bindString(&iamCfg.OwnerValue, "", "optional resource ownership tag value (defaults to 'owned')", "aws-owner-value", "owner-value")
	bindString(&iamCfg.SourcePrincipalARN, "", "optional IAM principal ARN allowed to AssumeRole into CAPA/controller/operator roles", "aws-source-principal-arn", "source-principal-arn")
	bindFunc("approved AMI ID for CAPA RunInstances (repeatable; defaults to cluster-tagged AMIs)", func(v string) error {
		iamCfg.AMIs = append(iamCfg.AMIs, v)
		return nil
	}, "aws-ami", "ami")
	bindFunc("approved Subnet ID for CAPA RunInstances (repeatable)", func(v string) error {
		iamCfg.Subnets = append(iamCfg.Subnets, v)
		return nil
	}, "aws-subnet", "subnet")
	bindFunc("approved Security Group ID for CAPA RunInstances (repeatable)", func(v string) error {
		iamCfg.SecurityGroups = append(iamCfg.SecurityGroups, v)
		return nil
	}, "aws-security-group", "security-group")
	bindString(&iamCfg.AssetBucket, "", "optional private S3 bucket for bootstrap binary presigning", "aws-asset-bucket", "asset-bucket")
	bindString(&iamCfg.AssetPrefix, "", "optional S3 key prefix in --aws-asset-bucket", "aws-asset-prefix", "asset-prefix")
	bindString(&iamCfg.VPCCNIFamily, "dual", "AWS VPC CNI address family: ipv4, ipv6, or dual", "aws-vpc-cni-family", "vpc-cni-family")
	bindBool(&iamCfg.IncludeAMILifecycle, true, "include AMI capture, registration, volume sizing, and NVIDIA GRID policies", "aws-include-ami-lifecycle", "include-ami-lifecycle")
	bindBool(&iamCfg.IncludeAMIPerformance, true, "include Windows EC2 Fast Launch and EBS snapshot warming policies", "aws-include-ami-performance", "include-ami-performance")
	bindBool(&iamCfg.IncludeObservability, true, "include worker CloudWatch/SSM telemetry and operator console/SSM diagnostic policies", "aws-include-observability", "include-observability")
	bindString(&iamCfg.ClusterRoleIdentity, "", "optional AWSClusterRoleIdentity name to create/update in Kubernetes during iam-apply", "aws-cluster-role-identity", "cluster-role-identity")
	bindString(&iamCfg.SourceIdentityName, "", "optional AWSClusterStaticIdentity name referenced by --aws-cluster-role-identity", "aws-source-identity", "source-identity")
	bindString(&iamCfg.AllowedNamespace, "cloud-provisioning", "namespace allowed to use --aws-cluster-role-identity", "aws-allowed-namespace", "allowed-namespace")

	var positionalVerb string
	if len(args) > 0 && IsCommand(args[0]) {
		positionalVerb = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 && positionalVerb == "" && IsCommand(fs.Arg(0)) {
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

	hasClusterEnv := func() bool {
		return *kubeconfig != "" || os.Getenv("KUBERNETES_SERVICE_HOST") != ""
	}

	newKubeClient := func() (client.Client, *rest.Config, error) {
		loading := clientcmd.NewDefaultClientConfigLoadingRules()
		if *kubeconfig != "" {
			loading = &clientcmd.ClientConfigLoadingRules{ExplicitPath: *kubeconfig}
		}
		cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, &clientcmd.ConfigOverrides{CurrentContext: *kubecontext}).ClientConfig()
		if err != nil {
			return nil, nil, err
		}
		cfg.Timeout = *timeout
		sc := runtime.NewScheme()
		_ = clientgoscheme.AddToScheme(sc)
		for _, kind := range []string{"IPPool", "BlockAffinity", "IPAMBlock", "BGPPeer", "BGPConfiguration", "IPAMConfig"} {
			gvk := schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: kind}
			sc.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
			sc.AddKnownTypeWithName(gvk.GroupVersion().WithKind(kind+"List"), &unstructured.UnstructuredList{})
		}
		for _, gvk := range []schema.GroupVersionKind{
			{Group: "gateway.nginx.org", Version: "v1alpha2", Kind: "NginxProxy"},
			{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"},
			{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"},
			{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"},
			{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSCluster"},
			{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSMachineTemplate"},
			{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSClusterRoleIdentity"},
		} {
			sc.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
			sc.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
		}
		cl, err := client.New(cfg, client.Options{Scheme: sc})
		return cl, cfg, err
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
		report := &tuple.ReconcileReport{}
		if hasClusterEnv() || len(manifestPaths) == 0 {
			cl, restCfg, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(ctx, *timeout)
			defer cancel()
			var writer client.Client = cl
			if *dryRun {
				writer = nil
			}
			report, err = tuple.ReconcileCalicoSite(ctx, writer, cl, winOpts.MTU)
			if err != nil {
				return err
			}
			if report.EffectiveMTU >= 1280 && restCfg != nil {
				clamped, vethChanges := clampCalicoNodeVeths(ctx, cl, restCfg, report.EffectiveMTU, *dryRun)
				report.VethsClamped += clamped
				report.Changes = append(report.Changes, vethChanges...)
			}
			if restCfg != nil {
				tsFixed, tsChanges := reconcileCalicoNodeTailscaleRules(ctx, cl, restCfg, *dryRun)
				report.TailscaleRulesFixed += tsFixed
				report.Changes = append(report.Changes, tsChanges...)
			}
		}
		if len(manifestPaths) > 0 {
			fileChanges, err := tuple.ReconcileGitOpsManifestFiles(manifestPaths, *dryRun)
			if err != nil {
				return err
			}
			if len(fileChanges) > 0 {
				report.ManifestFilesPatched = true
				report.Changes = append(report.Changes, fileChanges...)
			}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)

	case "iam-render":
		iamCfg.Tuple = *tupleExpr
		var reader client.Reader
		if hasClusterEnv() {
			if cl, _, err := newKubeClient(); err == nil {
				reader = cl
			} else if *kubeconfig != "" {
				return err
			}
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		if iamCfg.AccountID == "" {
			if _, acct, err := awsiam.NewClientFromDefaultConfig(ctx, iamCfg.Region); err == nil && acct != "" {
				iamCfg.AccountID = acct
			}
		}
		bundle, err := tuple.RenderIAMBundle(ctx, reader, iamCfg)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(bundle)

	case "iam-apply":
		iamCfg.Tuple = *tupleExpr
		var cl client.Client
		if hasClusterEnv() {
			var err error
			cl, _, err = newKubeClient()
			if err != nil && *kubeconfig != "" {
				return err
			}
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		iamClient, acct, err := awsiam.NewClientFromDefaultConfig(ctx, iamCfg.Region)
		if err != nil {
			return err
		}
		if iamCfg.AccountID == "" {
			iamCfg.AccountID = acct
		}
		report, _, err := tuple.ApplyIAMBundle(ctx, iamClient, cl, cl, iamCfg, *dryRun)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)

	case "iam-cleanup":
		iamCfg.Tuple = *tupleExpr
		var cl client.Client
		if hasClusterEnv() {
			var err error
			cl, _, err = newKubeClient()
			if err != nil && *kubeconfig != "" {
				return err
			}
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		iamClient, acct, err := awsiam.NewClientFromDefaultConfig(ctx, iamCfg.Region)
		if err != nil {
			return err
		}
		if iamCfg.AccountID == "" {
			iamCfg.AccountID = acct
		}
		report, _, err := tuple.CleanupIAMBundle(ctx, iamClient, cl, iamCfg, *dryRun)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)

	case "check":
		var reader client.Reader
		var nodeVethMTUs map[string]map[string]int
		var nodeTailscaleRules map[string]tuple.TailscaleRuleStatus
		if hasClusterEnv() {
			cl, restCfg, err := newKubeClient()
			if err != nil {
				if *kubeconfig != "" {
					return err
				}
			} else {
				reader = cl
				if restCfg != nil {
					checkCtx, checkCancel := context.WithTimeout(ctx, *timeout)
					nodeVethMTUs = collectCalicoNodeVethMTUs(checkCtx, cl, restCfg)
					nodeTailscaleRules = collectCalicoNodeTailscaleRules(checkCtx, cl, restCfg)
					checkCancel()
				}
			}
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
			NodeVethMTUs:       nodeVethMTUs,
			NodeTailscaleRules: nodeTailscaleRules,
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
		return fmt.Errorf("unknown --mode %q (expected check, fix, iam-render, iam-apply, iam-cleanup, render, k0s-network, or validate-k0s-config)", *mode)
	}
}

func execInCalicoNode(ctx context.Context, restCfg *rest.Config, podName string, cmd []string) (string, error) {
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return "", err
	}
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace("kube-system").
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "calico-node",
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, clientgoscheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return "", fmt.Errorf("%w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func collectCalicoNodeVethMTUs(ctx context.Context, reader client.Reader, restCfg *rest.Config) map[string]map[string]int {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace("kube-system"), client.MatchingLabels{"k8s-app": "calico-node"}); err != nil {
		return nil
	}
	out := map[string]map[string]int{}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
			continue
		}
		raw, err := execInCalicoNode(ctx, restCfg, pod.Name, []string{"ip", "-o", "link", "show"})
		if err != nil {
			continue
		}
		if veths := tuple.ParseCaliVethMTUs(raw); len(veths) > 0 {
			out[pod.Spec.NodeName] = veths
		}
	}
	return out
}

func clampCalicoNodeVeths(ctx context.Context, reader client.Reader, restCfg *rest.Config, targetMTU int, dryRun bool) (int, []string) {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace("kube-system"), client.MatchingLabels{"k8s-app": "calico-node"}); err != nil {
		return 0, nil
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].Spec.NodeName < pods.Items[j].Spec.NodeName
	})
	var totalClamped int
	var changes []string
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
			continue
		}
		raw, err := execInCalicoNode(ctx, restCfg, pod.Name, []string{"ip", "-o", "link", "show"})
		if err != nil {
			continue
		}
		veths := tuple.ParseCaliVethMTUs(raw)
		var staleNames []string
		for name, mtu := range veths {
			if mtu != targetMTU {
				staleNames = append(staleNames, name)
			}
		}
		sort.Strings(staleNames)
		if len(staleNames) == 0 {
			continue
		}
		if !dryRun {
			script := fmt.Sprintf("for dev in %s; do ip link set dev \"$dev\" mtu %d; done", strings.Join(staleNames, " "), targetMTU)
			if _, err := execInCalicoNode(ctx, restCfg, pod.Name, []string{"sh", "-c", script}); err != nil {
				continue
			}
		}
		totalClamped += len(staleNames)
		changes = append(changes, fmt.Sprintf("node %s (%s) clamped %d live cali* veth interface(s) to MTU %d", pod.Spec.NodeName, pod.Name, len(staleNames), targetMTU))
	}
	return totalClamped, changes
}

func collectCalicoNodeTailscaleRules(ctx context.Context, reader client.Reader, restCfg *rest.Config) map[string]tuple.TailscaleRuleStatus {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace("kube-system"), client.MatchingLabels{"k8s-app": "calico-node"}); err != nil {
		return nil
	}
	out := map[string]tuple.TailscaleRuleStatus{}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
			continue
		}
		links, err := execInCalicoNode(ctx, restCfg, pod.Name, []string{"ip", "-o", "link", "show"})
		if err != nil {
			continue
		}
		rulesV4, _ := execInCalicoNode(ctx, restCfg, pod.Name, []string{"ip", "-4", "rule", "show"})
		rulesV6, _ := execInCalicoNode(ctx, restCfg, pod.Name, []string{"ip", "-6", "rule", "show"})
		st := tuple.ParseTailscaleRuleStatus(links, rulesV4, rulesV6)
		if st.HasTailscale {
			out[pod.Spec.NodeName] = st
		}
	}
	return out
}

func reconcileCalicoNodeTailscaleRules(ctx context.Context, reader client.Reader, restCfg *rest.Config, dryRun bool) (int, []string) {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace("kube-system"), client.MatchingLabels{"k8s-app": "calico-node"}); err != nil {
		return 0, nil
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].Spec.NodeName < pods.Items[j].Spec.NodeName
	})
	var fixed int
	var changes []string
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
			continue
		}
		links, err := execInCalicoNode(ctx, restCfg, pod.Name, []string{"ip", "-o", "link", "show"})
		if err != nil {
			continue
		}
		rulesV4, _ := execInCalicoNode(ctx, restCfg, pod.Name, []string{"ip", "-4", "rule", "show"})
		rulesV6, _ := execInCalicoNode(ctx, restCfg, pod.Name, []string{"ip", "-6", "rule", "show"})
		st := tuple.ParseTailscaleRuleStatus(links, rulesV4, rulesV6)
		if !st.MissingV4() && !st.MissingV6() {
			continue
		}
		var families []string
		var cmds []string
		if st.MissingV4() {
			families = append(families, "IPv4 (100.64.0.0/10 lookup 52)")
			cmds = append(cmds, "ip -4 rule add priority 5200 to 100.64.0.0/10 lookup 52 || true")
		}
		if st.MissingV6() {
			families = append(families, "IPv6 (fd7a:115c:a1e0::/48 lookup 52)")
			cmds = append(cmds, "ip -6 rule add priority 5200 to fd7a:115c:a1e0::/48 lookup 52 || true")
		}
		if !dryRun {
			if _, err := execInCalicoNode(ctx, restCfg, pod.Name, []string{"sh", "-c", strings.Join(cmds, "; ")}); err != nil {
				continue
			}
		}
		fixed += len(families)
		changes = append(changes, fmt.Sprintf("node %s (%s) installed priority 5200 Tailscale bypass rule(s) ahead of Calico fwmark: %s", pod.Spec.NodeName, pod.Name, strings.Join(families, ", ")))
	}
	return fixed, changes
}
