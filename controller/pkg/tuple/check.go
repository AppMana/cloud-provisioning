package tuple

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni/site"
	joink0s "github.com/appmana/cloud-provisioning/controller/pkg/join/k0s"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// CheckResult is one named outcome of a platform tuple health/environment check.
type CheckResult struct {
	Name     string   `json:"name"`
	Pass     bool     `json:"pass"`
	Status   string   `json:"status,omitempty"`
	Detail   string   `json:"detail"`
	Warnings []string `json:"warnings,omitempty"`
}

// EffectiveStatus returns "PASS", "WARN", or "FAIL" for this check result.
func (r CheckResult) EffectiveStatus() string {
	if !r.Pass {
		return "FAIL"
	}
	if len(r.Warnings) > 0 || r.Status == "WARN" {
		return "WARN"
	}
	return "PASS"
}

// CheckOptions configures Evaluate for a target platform tuple and cluster.
type CheckOptions struct {
	// Tuple is the platform tuple string, e.g. "k0s + calico,bgp + aws".
	Tuple string
	// K0sConfig is optional raw k0s.yaml or k0sctl.yaml bytes to validate.
	K0sConfig []byte
	// ValuesYAML is optional Helm values.yaml bytes for charts/cloud-provisioning.
	ValuesYAML []byte
	// ManifestsYAML is optional Kubernetes manifest YAML documents to inspect
	// (ProvisionedNodeClass, BGPPeer, Deployment, etc.).
	ManifestsYAML [][]byte
	// TunnelIPv4 is the planned IPv4 WireGuard tunnel subnet (default 10.100.0.0/24).
	TunnelIPv4 string
	// TunnelIPv6 is the planned IPv6 WireGuard tunnel subnet (default fd00:10:100::/96).
	TunnelIPv6 string
	// TunnelMTU is the unfragmented WireGuard payload ceiling (default 1420).
	TunnelMTU int
	// RequireDualStack requires both IPv4 and IPv6 pod CIDRs when true.
	RequireDualStack bool
	// RequireWindowsBGP requires calico-node-windows, calico-windows-config-actual,
	// and IPAMConfig/default to be present even if no Windows Node has joined yet.
	RequireWindowsBGP bool
	// StrictBGPPeerScope requires BGPPeers to explicitly exclude cloud-worker
	// even when cloud workers use aws-vpc native CNI rather than site CNI.
	StrictBGPPeerScope bool
	// NodeVethMTUs is an optional map of nodeName -> (cali* veth name -> live MTU)
	// used to detect pre-existing pod veth interfaces that still carry a stale MTU.
	NodeVethMTUs map[string]map[string]int
}

// Passed reports whether every CheckResult succeeded (including checks with warnings).
func Passed(results []CheckResult) bool {
	for _, r := range results {
		if !r.Pass {
			return false
		}
	}
	return len(results) > 0
}

// Warnings returns all warning messages emitted across the evaluated CheckResults.
func Warnings(results []CheckResult) []string {
	var out []string
	for _, r := range results {
		for _, w := range r.Warnings {
			w = strings.TrimSpace(w)
			if w == "" {
				continue
			}
			out = append(out, fmt.Sprintf("%s: %s", r.Name, w))
		}
	}
	return out
}

// FormatReport renders a human-readable summary of all CheckResults, including
// PASS/WARN/FAIL status and any per-check warnings.
func FormatReport(results []CheckResult) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "%-4s  %-24s  %s\n", r.EffectiveStatus(), r.Name, r.Detail)
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "      warning: %s\n", w)
		}
	}
	return b.String()
}

func normalizeResults(results []CheckResult) []CheckResult {
	for i := range results {
		results[i].Status = results[i].EffectiveStatus()
	}
	return results
}

// LoadManifestBytes reads one or more YAML manifest file paths or directories
// (comma-separated or repeated) into raw document slices for CheckOptions.ManifestsYAML.
func LoadManifestBytes(paths []string) ([][]byte, error) {
	var out [][]byte
	for _, rawPath := range paths {
		for _, part := range strings.Split(rawPath, ",") {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			info, err := os.Stat(p)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() {
				b, err := os.ReadFile(p)
				if err != nil {
					return nil, err
				}
				out = append(out, b)
				continue
			}
			entries, err := os.ReadDir(p)
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				ext := strings.ToLower(filepath.Ext(entry.Name()))
				if ext != ".yaml" && ext != ".yml" && ext != ".json" {
					continue
				}
				b, err := os.ReadFile(filepath.Join(p, entry.Name()))
				if err != nil {
					return nil, err
				}
				out = append(out, b)
			}
		}
	}
	return out, nil
}

// Evaluate runs the full health and environment check for the chosen platform tuple.
func Evaluate(ctx context.Context, c client.Reader, opts CheckOptions) ([]CheckResult, error) {
	if opts.TunnelIPv4 == "" {
		opts.TunnelIPv4 = "10.100.0.0/24"
	}
	if opts.TunnelIPv6 == "" {
		opts.TunnelIPv6 = "fd00:10:100::/96"
	}
	if opts.TunnelMTU == 0 {
		opts.TunnelMTU = 1420
	}
	tupleStr := strings.TrimSpace(opts.Tuple)
	if tupleStr == "" {
		tupleStr = "k0s + calico,bgp + aws"
	}

	var out []CheckResult
	parsedTuple, err := Parse(tupleStr)
	if err != nil {
		out = append(out, CheckResult{Name: "tuple", Pass: false, Detail: err.Error()})
		return normalizeResults(out), nil
	}
	out = append(out, CheckResult{
		Name:   "tuple",
		Pass:   true,
		Detail: fmt.Sprintf("%s (os=%s cloudCNI=%s)", parsedTuple, parsedTuple.GuestOS, parsedTuple.CloudCNI),
	})

	if len(opts.K0sConfig) > 0 {
		warnings, err := InspectK0sCalicoConfig(opts.K0sConfig, parsedTuple, opts.TunnelMTU)
		if err != nil {
			out = append(out, CheckResult{Name: "k0s-config", Pass: false, Detail: err.Error()})
		} else {
			out = append(out, CheckResult{
				Name:     "k0s-config",
				Pass:     true,
				Detail:   "allocate-node-cidrs=false, calico bird/Never, and MTU aligned with FELIX_*MTU",
				Warnings: warnings,
			})
		}
	}

	if len(opts.ValuesYAML) > 0 {
		out = append(out, checkHelmValues(opts.ValuesYAML, parsedTuple))
	}

	if len(opts.ManifestsYAML) > 0 {
		out = append(out, checkManifests(opts.ManifestsYAML, parsedTuple))
	}

	if c == nil {
		return normalizeResults(out), nil
	}

	network, err := cni.Detect(ctx, c)
	if err != nil {
		out = append(out, CheckResult{Name: "network", Pass: false, Detail: err.Error()})
		return normalizeResults(out), nil
	}

	expectedEncap := parsedTuple.ExpectedEncapsulation()
	cniPass := network.Name == parsedTuple.CNI && network.Encapsulation == expectedEncap
	cniDetail := fmt.Sprintf("cni=%s encapsulation=%s (%s)", network.Name, network.Encapsulation, network.Detail)
	if !cniPass {
		cniDetail = fmt.Sprintf("detected cni=%s encapsulation=%s; tuple %s requires cni=%s encapsulation=%s", network.Name, network.Encapsulation, parsedTuple, parsedTuple.CNI, expectedEncap)
	}
	out = append(out, CheckResult{Name: "network", Pass: cniPass, Detail: cniDetail})

	dualStack, err := network.DualStack(ctx, c)
	if err != nil {
		return nil, err
	}
	dualPass := true
	if opts.RequireDualStack {
		dualPass = dualStack
	}
	out = append(out, CheckResult{
		Name:   "dual-stack",
		Pass:   dualPass,
		Detail: fmt.Sprintf("dualStack=%t", dualStack),
	})

	windowsPresent, err := hasWindowsNodeOrDaemonSet(ctx, c)
	if err != nil {
		return nil, err
	}
	if parsedTuple.CNI == cni.Calico {
		out = append(out, checkCalicoMTU(ctx, c, parsedTuple, opts.TunnelMTU, opts.NodeVethMTUs))
		out = append(out, checkCalicoAutodetection(ctx, c, opts.TunnelIPv4, opts.TunnelIPv6, dualStack || opts.RequireDualStack))
		out = append(out, checkCalicoIPAM(ctx, c, opts.RequireWindowsBGP || windowsPresent))
	}

	if parsedTuple.CNI == cni.Calico && (opts.RequireWindowsBGP || parsedTuple.IncludesWindows()) {
		winRes := checkWindowsBGP(ctx, c, dualStack || opts.RequireDualStack)
		if !winRes.Pass && !opts.RequireWindowsBGP && !windowsPresent {
			winRes.Pass = true
			winRes.Status = "WARN"
			winRes.Warnings = []string{fmt.Sprintf(
				"tuple %s includes Windows nodes, but Windows BGP resources are not yet installed (%s); endpoint-controller or 'platform-tuple fix' will reconcile them before Windows nodes join",
				parsedTuple, winRes.Detail,
			)}
		}
		out = append(out, winRes)
	}
	if parsedTuple.Distribution == "k0s" && parsedTuple.IncludesWindows() {
		if err := joink0s.WindowsEgressReady(ctx, c); err != nil {
			out = append(out, CheckResult{Name: "k0s-windows-egress", Pass: false, Detail: err.Error()})
		} else {
			out = append(out, CheckResult{Name: "k0s-windows-egress", Pass: true, Detail: "konnectivity-agent egress safe for Windows workers"})
		}
	}

	out = append(out, checkSiteCNIExclusion(ctx, c, network, parsedTuple, windowsPresent))
	out = append(out, checkBGPPeers(ctx, c, parsedTuple, opts.StrictBGPPeerScope))
	out = append(out, checkMasqueradeAndProviderPools(ctx, c, network))
	out = append(out, checkBorrowedAddresses(ctx, c))
	out = append(out, checkTunnelRanges(ctx, c, dualStack, opts.TunnelIPv4, opts.TunnelIPv6))
	out = append(out, checkEndpointCandidates(ctx, c))
	if workloadRes, ok := checkCloudWorkerWorkloads(ctx, c); ok {
		out = append(out, workloadRes)
	}

	return normalizeResults(out), nil
}

// VerifyClaimEnvironment is the runtime preflight called by endpoint-controller's
// claim.Reconciler before admitting or provisioning a cloud worker.
func VerifyClaimEnvironment(ctx context.Context, c client.Reader, joinProvider string, network cni.Network, class platform.Class, excluder *site.Excluder) error {
	tupleSpec := Tuple{
		Distribution:  strings.ToLower(strings.TrimSpace(joinProvider)),
		CNI:           network.Name,
		Encapsulation: network.Encapsulation.String(),
		Provider:      "aws",
		GuestOS:       class.OS,
		CloudCNI:      class.CNI,
	}
	if tupleSpec.Encapsulation == "native" {
		tupleSpec.Encapsulation = "bgp"
	}
	if err := tupleSpec.Validate(); err != nil {
		return err
	}
	if excluder != nil {
		if err := excluder.Verify(ctx, network, class); err != nil {
			return err
		}
	}
	if class.OS == "windows" && joinProvider == "k0s" {
		if err := joink0s.WindowsEgressReady(ctx, c); err != nil {
			return err
		}
	}
	if class.OS == "windows" && class.CNI == "site" && network.Name == cni.Calico {
		dualStack, _ := network.DualStack(ctx, c)
		res := checkWindowsBGP(ctx, c, dualStack)
		if !res.Pass {
			return fmt.Errorf("windows-bgp preflight: %s", res.Detail)
		}
	}
	return nil
}

// ParseCaliVethMTUs parses `ip -o link show` output and returns a map of
// Calico host veth interface names (cali*) to their current MTU.
func ParseCaliVethMTUs(ipLinkOutput string) map[string]int {
	return site.ParseCaliVethMTUs(ipLinkOutput)
}

// ReconcileGitOpsManifestFiles inspects YAML manifest files or directories on
// disk (e.g. a Flux or ArgoCD GitOps repository checkout) and automatically
// remediates NginxProxy hostPort Recreate patches and HelmRelease/DaemonSet
// cloud-worker exclusions.
func ReconcileGitOpsManifestFiles(paths []string, dryRun bool) ([]string, error) {
	return site.ReconcileGitOpsManifestFiles(paths, dryRun)
}

func checkCalicoMTU(ctx context.Context, c client.Reader, parsedTuple Tuple, tunnelMTU int, nodeVethMTUs map[string]map[string]int) CheckResult {
	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: LinuxDaemonSetName}, &ds); err != nil {
		return CheckResult{Name: "calico-mtu", Pass: false, Detail: fmt.Sprintf("read kube-system/calico-node: %v", err)}
	}
	cniMTU, source, err := cni.CalicoPodMTU(ctx, c, &ds)
	if err != nil {
		return CheckResult{Name: "calico-mtu", Pass: false, Detail: err.Error()}
	}
	if cniMTU < 1280 || cniMTU > 65535 {
		return CheckResult{Name: "calico-mtu", Pass: false, Detail: fmt.Sprintf("CNI MTU %d (%s) is outside usable dual-stack range 1280-65535", cniMTU, source)}
	}
	felixMTUs := map[string]int{}
	for _, container := range ds.Spec.Template.Spec.Containers {
		if container.Name != LinuxDaemonSetName {
			continue
		}
		for _, env := range container.Env {
			switch env.Name {
			case "FELIX_IPINIPMTU", "FELIX_VXLANMTU", "FELIX_WIREGUARDMTU":
				val, err := strconv.Atoi(env.Value)
				if err != nil || val < 1280 {
					return CheckResult{Name: "calico-mtu", Pass: false, Detail: fmt.Sprintf("kube-system/calico-node has invalid %s=%q", env.Name, env.Value)}
				}
				felixMTUs[env.Name] = val
			}
		}
	}
	for _, key := range []string{"FELIX_IPINIPMTU", "FELIX_VXLANMTU", "FELIX_WIREGUARDMTU"} {
		if fMTU, ok := felixMTUs[key]; ok && fMTU != cniMTU {
			return CheckResult{
				Name:   "calico-mtu",
				Pass:   false,
				Detail: fmt.Sprintf("kube-system/calico-node CNI_NETWORK_CONFIG mtu (%d) disagrees with %s=%d (set spec.network.calico.mtu=%d in k0s config so 10-calico.conflist does not stay at k0s default 1450)", cniMTU, key, fMTU, fMTU),
			}
		}
	}

	var winCM corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: WindowsConfigMapName}, &winCM); err == nil {
		if winMTUStr, ok := winCM.Data["CNI_MTU"]; ok && strings.TrimSpace(winMTUStr) != "" {
			winMTU, err := strconv.Atoi(strings.TrimSpace(winMTUStr))
			if err != nil || winMTU != cniMTU {
				return CheckResult{
					Name:   "calico-mtu",
					Pass:   false,
					Detail: fmt.Sprintf("kube-system/%s CNI_MTU=%q disagrees with Linux calico-node mtu=%d", WindowsConfigMapName, winMTUStr, cniMTU),
				}
			}
		}
	}

	if parsedTuple.CloudCNI == "site" && tunnelMTU > 0 && cniMTU > tunnelMTU {
		return CheckResult{
			Name:   "calico-mtu",
			Pass:   false,
			Detail: fmt.Sprintf("calico pod MTU %d (%s) > tunnel MTU %d for cloudCNI=site", cniMTU, source, tunnelMTU),
		}
	}
	var warnings []string
	if parsedTuple.CloudCNI == "aws-vpc" && tunnelMTU > 0 && cniMTU > tunnelMTU {
		warnings = append(warnings, fmt.Sprintf(
			"site Calico pod MTU (%d) exceeds WireGuard tunnel MTU (%d); aws-node clamps aws-vpc cloud-worker pods to %d while site pods use %d",
			cniMTU, tunnelMTU, tunnelMTU, cniMTU,
		))
	}
	if len(nodeVethMTUs) > 0 {
		var nodeNames []string
		for n := range nodeVethMTUs {
			nodeNames = append(nodeNames, n)
		}
		sort.Strings(nodeNames)
		for _, nodeName := range nodeNames {
			veths := nodeVethMTUs[nodeName]
			staleCounts := map[int]int{}
			for _, vMTU := range veths {
				if vMTU != cniMTU {
					staleCounts[vMTU]++
				}
			}
			var staleMTUs []int
			for m := range staleCounts {
				staleMTUs = append(staleMTUs, m)
			}
			sort.Ints(staleMTUs)
			for _, m := range staleMTUs {
				warnings = append(warnings, fmt.Sprintf(
					"node %s has %d live cali* veth interface(s) at stale MTU %d != CNI MTU %d (existing pods retain pre-update veth MTU; run 'platform-tuple fix' to clamp in-place without pod restarts)",
					nodeName, staleCounts[m], m, cniMTU,
				))
			}
		}
	}
	return CheckResult{
		Name:     "calico-mtu",
		Pass:     true,
		Detail:   fmt.Sprintf("calico CNI and Felix MTU aligned at %d (%s, cloudCNI=%s tunnelMTU=%d)", cniMTU, source, parsedTuple.CloudCNI, tunnelMTU),
		Warnings: warnings,
	}
}

func checkCalicoAutodetection(ctx context.Context, c client.Reader, tunnelV4, tunnelV6 string, dualStack bool) CheckResult {
	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: LinuxDaemonSetName}, &ds); err != nil {
		return CheckResult{Name: "calico-autodetection", Pass: false, Detail: fmt.Sprintf("read kube-system/calico-node: %v", err)}
	}
	var v4Method, v6Method string
	for _, container := range ds.Spec.Template.Spec.Containers {
		if container.Name != LinuxDaemonSetName {
			continue
		}
		for _, env := range container.Env {
			if env.Name == "IP_AUTODETECTION_METHOD" {
				v4Method = strings.TrimSpace(env.Value)
			}
			if env.Name == "IP6_AUTODETECTION_METHOD" {
				v6Method = strings.TrimSpace(env.Value)
			}
		}
	}
	if err := validateAutodetectMethod("calico-node IP_AUTODETECTION_METHOD", v4Method, tunnelV4); err != nil {
		return CheckResult{Name: "calico-autodetection", Pass: false, Detail: err.Error()}
	}
	if dualStack && v6Method != "" {
		if err := validateAutodetectMethod("calico-node IP6_AUTODETECTION_METHOD", v6Method, tunnelV6); err != nil {
			return CheckResult{Name: "calico-autodetection", Pass: false, Detail: err.Error()}
		}
	}
	return CheckResult{
		Name:   "calico-autodetection",
		Pass:   true,
		Detail: fmt.Sprintf("v4=%q v6=%q", v4Method, v6Method),
	}
}

func validateAutodetectMethod(label, method, tunnelCIDR string) error {
	if method == "" || method == "first-found" {
		return fmt.Errorf("%s must be explicitly configured (not empty or first-found) so Calico never binds BGP to wg-cloud or transient adapters", label)
	}
	if strings.HasPrefix(method, "cidr=") {
		tunnelPrefix, err := netip.ParsePrefix(tunnelCIDR)
		if err == nil {
			for _, raw := range strings.Split(strings.TrimPrefix(method, "cidr="), ",") {
				p, err := netip.ParsePrefix(strings.TrimSpace(raw))
				if err != nil {
					return fmt.Errorf("%s has invalid CIDR %q: %w", label, raw, err)
				}
				if p.Overlaps(tunnelPrefix) {
					return fmt.Errorf("%s CIDR %s overlaps tunnel subnet %s", label, p, tunnelPrefix)
				}
			}
		}
	}
	return nil
}

func hasWindowsNodeOrDaemonSet(ctx context.Context, c client.Reader) (bool, error) {
	var ds appsv1.DaemonSet
	err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: WindowsDaemonSetName}, &ds)
	if err == nil {
		return true, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return false, err
	}
	for _, n := range nodes.Items {
		if n.Labels["kubernetes.io/os"] == "windows" && n.Labels[platform.RoleLabel] != platform.RoleValue {
			return true, nil
		}
	}
	return false, nil
}

func checkWindowsBGP(ctx context.Context, c client.Reader, dualStack bool) CheckResult {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: WindowsConfigMapName}, &cm); err != nil {
		return CheckResult{Name: "windows-bgp", Pass: false, Detail: fmt.Sprintf("missing kube-system/%s: %v", WindowsConfigMapName, err)}
	}
	requiredKeys := map[string]string{
		"CALICO_NETWORKING_BACKEND": "windows-bgp",
		"CALICO_DSR_DISABLE":        "true",
		"FELIX_HEALTHENABLED":       "true",
		"IP":                        "autodetect",
		"KUBECONFIG":                `c:\etc\cni\net.d\calico-kubeconfig`,
	}
	for k, want := range requiredKeys {
		if cm.Data[k] != want {
			return CheckResult{Name: "windows-bgp", Pass: false, Detail: fmt.Sprintf("kube-system/%s has %s=%q, want %q", WindowsConfigMapName, k, cm.Data[k], want)}
		}
	}
	if strings.TrimSpace(cm.Data["IP_AUTODETECTION_METHOD"]) == "" || strings.TrimSpace(cm.Data["IP_AUTODETECTION_METHOD"]) == "first-found" {
		return CheckResult{Name: "windows-bgp", Pass: false, Detail: fmt.Sprintf("kube-system/%s requires explicit IP_AUTODETECTION_METHOD (got %q)", WindowsConfigMapName, cm.Data["IP_AUTODETECTION_METHOD"])}
	}
	if dualStack {
		if cm.Data["IP6"] != "autodetect" || cm.Data["FELIX_IPV6SUPPORT"] != "true" || strings.TrimSpace(cm.Data["IP6_AUTODETECTION_METHOD"]) == "" {
			return CheckResult{Name: "windows-bgp", Pass: false, Detail: fmt.Sprintf("kube-system/%s missing dual-stack IP6=autodetect, FELIX_IPV6SUPPORT=true, or IP6_AUTODETECTION_METHOD", WindowsConfigMapName)}
		}
	}

	ipam := &unstructured.Unstructured{}
	ipam.SetGroupVersionKind(schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "IPAMConfig"})
	if err := c.Get(ctx, types.NamespacedName{Name: "default"}, ipam); err != nil {
		return CheckResult{Name: "windows-bgp", Pass: false, Detail: fmt.Sprintf("missing IPAMConfig/default: %v", err)}
	}
	for _, field := range []string{"strictAffinity", "autoAllocateBlocks"} {
		val, found, _ := unstructured.NestedBool(ipam.Object, "spec", field)
		if !found || !val {
			return CheckResult{Name: "windows-bgp", Pass: false, Detail: fmt.Sprintf("IPAMConfig/default must set spec.%s=true", field)}
		}
	}

	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: WindowsDaemonSetName}, &ds); err != nil {
		return CheckResult{Name: "windows-bgp", Pass: false, Detail: fmt.Sprintf("missing kube-system/%s: %v", WindowsDaemonSetName, err)}
	}
	pod := ds.Spec.Template.Spec
	if !pod.HostNetwork || pod.SecurityContext == nil || pod.SecurityContext.WindowsOptions == nil || pod.SecurityContext.WindowsOptions.HostProcess == nil || !*pod.SecurityContext.WindowsOptions.HostProcess {
		return CheckResult{Name: "windows-bgp", Pass: false, Detail: "calico-node-windows must run as a HostNetwork Windows HostProcess pod"}
	}
	containers := map[string]corev1.Container{}
	for _, container := range pod.Containers {
		containers[container.Name] = container
		usesActual := false
		for _, ef := range container.EnvFrom {
			if ef.ConfigMapRef != nil && ef.ConfigMapRef.Name == WindowsConfigMapName {
				usesActual = true
			}
		}
		if !usesActual {
			return CheckResult{Name: "windows-bgp", Pass: false, Detail: fmt.Sprintf("calico-node-windows container %q must reference %s (not k0s-owned %s)", container.Name, WindowsConfigMapName, StockWindowsConfigMap)}
		}
	}
	for _, req := range []string{"node", "felix", "confd"} {
		if _, ok := containers[req]; !ok {
			return CheckResult{Name: "windows-bgp", Pass: false, Detail: fmt.Sprintf("calico-node-windows is missing container %q", req)}
		}
	}
	nodeC := containers["node"]
	if nodeC.ReadinessProbe == nil || nodeC.ReadinessProbe.Exec == nil || !strings.Contains(strings.Join(nodeC.ReadinessProbe.Exec.Command, " "), "Test-CalicoBGPNodeReady") {
		return CheckResult{Name: "windows-bgp", Pass: false, Detail: "calico-node-windows node container is missing Test-CalicoBGPNodeReady readinessProbe"}
	}
	felixC := containers["felix"]
	if felixC.StartupProbe == nil || felixC.LivenessProbe == nil || felixC.ReadinessProbe == nil {
		return CheckResult{Name: "windows-bgp", Pass: false, Detail: "calico-node-windows felix container requires startupProbe, livenessProbe, and readinessProbe"}
	}

	return CheckResult{Name: "windows-bgp", Pass: true, Detail: "calico-windows-config-actual, IPAMConfig/default, and calico-node-windows verified"}
}

func checkCalicoIPAM(ctx context.Context, c client.Reader, strict bool) CheckResult {
	ipam := &unstructured.Unstructured{}
	ipam.SetGroupVersionKind(schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "IPAMConfig"})
	if err := c.Get(ctx, types.NamespacedName{Name: "default"}, ipam); err != nil {
		if strict {
			return CheckResult{Name: "calico-ipam", Pass: false, Detail: fmt.Sprintf("missing IPAMConfig/default: %v", err)}
		}
		return CheckResult{
			Name:     "calico-ipam",
			Pass:     true,
			Status:   "WARN",
			Detail:   fmt.Sprintf("missing IPAMConfig/default: %v", err),
			Warnings: []string{"IPAMConfig/default is not present; run 'platform-tuple fix' to set spec.strictAffinity=true and spec.autoAllocateBlocks=true"},
		}
	}
	var bad []string
	for _, field := range []string{"strictAffinity", "autoAllocateBlocks"} {
		val, found, _ := unstructured.NestedBool(ipam.Object, "spec", field)
		if !found || !val {
			bad = append(bad, fmt.Sprintf("spec.%s=%v", field, val))
		}
	}
	if len(bad) > 0 {
		detail := fmt.Sprintf("IPAMConfig/default has %s (want strictAffinity=true, autoAllocateBlocks=true)", strings.Join(bad, ", "))
		if strict {
			return CheckResult{Name: "calico-ipam", Pass: false, Detail: detail}
		}
		return CheckResult{
			Name:     "calico-ipam",
			Pass:     true,
			Status:   "WARN",
			Detail:   detail,
			Warnings: []string{detail + "; unencapsulated Calico BGP requires strictAffinity=true so pods never borrow IPs across nodes (run 'platform-tuple fix')"},
		}
	}
	return CheckResult{Name: "calico-ipam", Pass: true, Detail: "IPAMConfig/default strictAffinity=true autoAllocateBlocks=true"}
}

func checkSiteCNIExclusion(ctx context.Context, c client.Reader, network cni.Network, parsedTuple Tuple, windowsPresent bool) CheckResult {
	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: LinuxDaemonSetName}, &ds); err != nil {
		return CheckResult{Name: "site-cni-exclusion", Pass: false, Detail: err.Error()}
	}
	hasLinuxExclusion := daemonSetExcludesCloudRole(&ds)
	hasWindowsExclusion := true
	if windowsPresent {
		var winDS appsv1.DaemonSet
		if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: WindowsDaemonSetName}, &winDS); err == nil {
			hasWindowsExclusion = daemonSetExcludesCloudRole(&winDS)
		}
	}
	var wildcardWarnings []string
	wildcardWarnings = append(wildcardWarnings, site.CheckGitOpsDaemonSetExclusion(ctx, c, &ds, platform.RoleLabel, platform.RoleValue)...)
	var allDS appsv1.DaemonSetList
	if err := c.List(ctx, &allDS); err == nil {
		for i := range allDS.Items {
			item := &allDS.Items[i]
			if item.Namespace == "kube-system" || item.Labels[platform.ComponentLabel] != "" {
				continue
			}
			if daemonSetToleratesAllTaints(item) {
				if !daemonSetExcludesCloudRole(item) {
					wildcardWarnings = append(wildcardWarnings, fmt.Sprintf(
						"DaemonSet %s/%s has a wildcard taint toleration (operator=Exists) without excluding %s=%s; it will schedule onto remote WAN cloud workers unless excluded (run 'platform-tuple fix')",
						item.Namespace, item.Name, platform.RoleLabel, platform.RoleValue,
					))
				} else {
					wildcardWarnings = append(wildcardWarnings, site.CheckGitOpsDaemonSetExclusion(ctx, c, item, platform.RoleLabel, platform.RoleValue)...)
				}
			}
		}
	}
	if hasLinuxExclusion && hasWindowsExclusion {
		return CheckResult{
			Name:     "site-cni-exclusion",
			Pass:     true,
			Detail:   "site CNI DaemonSets exclude cloud-worker and native CNI nodes",
			Warnings: wildcardWarnings,
		}
	}
	// If no cloud-worker nodes are present yet, site.Excluder will reconcile
	// the nodeAffinity on endpoint-controller startup.
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return CheckResult{Name: "site-cni-exclusion", Pass: false, Detail: err.Error()}
	}
	for _, n := range nodes.Items {
		if n.Labels[platform.RoleLabel] == platform.RoleValue {
			var native []string
			if parsedTuple.CloudCNI != "" && parsedTuple.CloudCNI != platform.SiteCNI {
				native = []string{parsedTuple.CloudCNI}
			}
			ex := &site.Excluder{Reader: c, Native: native}
			class := platform.Class{OS: n.Labels["kubernetes.io/os"], CNI: parsedTuple.CloudCNI}
			if class.OS == "" {
				class.OS = "linux"
			}
			if err := ex.Verify(ctx, network, class); err != nil {
				return CheckResult{Name: "site-cni-exclusion", Pass: false, Detail: err.Error()}
			}
		}
	}
	warnings := append([]string{"site CNI DaemonSets do not yet exclude cloud-worker via nodeAffinity; endpoint-controller Excluder or 'platform-tuple fix' will reconcile this before cloud workers join"}, wildcardWarnings...)
	return CheckResult{
		Name:     "site-cni-exclusion",
		Pass:     true,
		Detail:   "no cloud-worker nodes present yet; endpoint-controller Excluder will apply nodeAffinity",
		Warnings: warnings,
	}
}

func daemonSetToleratesAllTaints(ds *appsv1.DaemonSet) bool {
	for _, t := range ds.Spec.Template.Spec.Tolerations {
		if t.Operator == corev1.TolerationOpExists && strings.TrimSpace(t.Key) == "" &&
			(t.Effect == "" || t.Effect == corev1.TaintEffectNoSchedule) {
			return true
		}
	}
	return false
}

func daemonSetExcludesCloudRole(ds *appsv1.DaemonSet) bool {
	aff := ds.Spec.Template.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	for _, term := range aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == platform.RoleLabel && expr.Operator == corev1.NodeSelectorOpNotIn {
				return true
			}
		}
	}
	return false
}

func checkBGPPeers(ctx context.Context, c client.Reader, parsedTuple Tuple, strict bool) CheckResult {
	peers, err := site.PeersReachingCloudNodes(ctx, c)
	if err != nil {
		return CheckResult{Name: "bgp-peers", Pass: false, Detail: err.Error()}
	}
	if len(peers) == 0 {
		return CheckResult{Name: "bgp-peers", Pass: true, Detail: "router BGPPeers exclude cloud-worker"}
	}
	if parsedTuple.CloudCNI == "site" || strict {
		return CheckResult{
			Name:   "bgp-peers",
			Pass:   false,
			Detail: fmt.Sprintf("BGPPeer(s) %v select cloud-worker nodes without a router peering path", peers),
		}
	}
	return CheckResult{
		Name:   "bgp-peers",
		Pass:   true,
		Detail: fmt.Sprintf("BGPPeer(s) %v select by OS; safe for cloudCNI=%s because calico-node is excluded from %s nodes", peers, parsedTuple.CloudCNI, parsedTuple.CloudCNI),
		Warnings: []string{fmt.Sprintf(
			"BGPPeer(s) %v select nodes without '!has(%s)'; recommend adding '!has(%s)' to nodeSelector so cloud-worker nodes never peer with site routers",
			peers, platform.RoleLabel, platform.RoleLabel,
		)},
	}
}

func checkMasqueradeAndProviderPools(ctx context.Context, c client.Reader, network cni.Network) CheckResult {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return CheckResult{Name: "masquerade", Pass: false, Detail: err.Error()}
	}
	var blocks []string
	for _, n := range nodes.Items {
		var prefixes []netip.Prefix
		if n.Labels[platform.CNILabel] == "aws-vpc" {
			for _, raw := range append([]string{n.Spec.PodCIDR}, n.Spec.PodCIDRs...) {
				if p, err := netip.ParsePrefix(strings.TrimSpace(raw)); err == nil {
					prefixes = append(prefixes, p)
				}
			}
		} else {
			p, err := network.PrefixesFor(ctx, c, n.Name)
			if err != nil {
				continue
			}
			prefixes = p
		}
		if len(prefixes) == 0 {
			continue
		}
		if err := network.CheckMasquerade(ctx, c, prefixes); err != nil {
			return CheckResult{Name: "masquerade", Pass: false, Detail: err.Error()}
		}
		for _, p := range prefixes {
			blocks = append(blocks, n.Name+" "+p.String())
		}
	}
	return CheckResult{Name: "masquerade", Pass: true, Detail: "pod sources preserved for " + strings.Join(blocks, ", ")}
}

func checkBorrowedAddresses(ctx context.Context, c client.Reader) CheckResult {
	blocks := &unstructured.UnstructuredList{}
	blocks.SetGroupVersionKind(schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "IPAMBlockList"})
	if err := c.List(ctx, blocks); err != nil {
		return CheckResult{Name: "borrowed-addresses", Pass: true, Detail: "IPAMBlockList not present"}
	}
	borrowed := map[string]int{}
	for _, b := range blocks.Items {
		affinity, _, _ := unstructured.NestedString(b.Object, "spec", "affinity")
		host := strings.TrimPrefix(affinity, "host:")
		attributes, _, _ := unstructured.NestedSlice(b.Object, "spec", "attributes")
		allocations, _, _ := unstructured.NestedSlice(b.Object, "spec", "allocations")
		for _, a := range allocations {
			index, ok := a.(int64)
			if !ok || index < 0 || int(index) >= len(attributes) {
				continue
			}
			attr, _ := attributes[index].(map[string]any)
			secondary, _ := attr["secondary"].(map[string]any)
			if node, _ := secondary["node"].(string); node != "" && node != host {
				borrowed[node]++
			}
		}
	}
	if len(borrowed) == 0 {
		return CheckResult{Name: "borrowed-addresses", Pass: true, Detail: fmt.Sprintf("no borrowed IPAM block addresses (%d blocks)", len(blocks.Items))}
	}
	var parts []string
	for node, n := range borrowed {
		parts = append(parts, fmt.Sprintf("%s:%d", node, n))
	}
	sort.Strings(parts)
	return CheckResult{Name: "borrowed-addresses", Pass: false, Detail: "borrowed IPAM addresses: " + strings.Join(parts, ", ")}
}

func checkTunnelRanges(ctx context.Context, c client.Reader, dualStack bool, tunnelV4, tunnelV6 string) CheckResult {
	t4, err := netip.ParsePrefix(tunnelV4)
	if err != nil {
		return CheckResult{Name: "tunnel-ranges", Pass: false, Detail: err.Error()}
	}
	ranges := []netip.Prefix{t4}
	if dualStack {
		t6, err := netip.ParsePrefix(tunnelV6)
		if err != nil {
			return CheckResult{Name: "tunnel-ranges", Pass: false, Detail: err.Error()}
		}
		ranges = append(ranges, t6)
	}
	type used struct{ what, value string }
	var uses []used
	pools := &unstructured.UnstructuredList{}
	pools.SetGroupVersionKind(schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "IPPoolList"})
	if err := c.List(ctx, pools); err == nil {
		for _, p := range pools.Items {
			cidr, _, _ := unstructured.NestedString(p.Object, "spec", "cidr")
			uses = append(uses, used{"pool " + p.GetName(), cidr})
		}
	}
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err == nil {
		for _, n := range nodes.Items {
			for _, a := range n.Status.Addresses {
				if a.Type == corev1.NodeInternalIP || a.Type == corev1.NodeExternalIP {
					uses = append(uses, used{n.Name + " " + string(a.Type), a.Address})
				}
			}
			for _, cidr := range n.Spec.PodCIDRs {
				uses = append(uses, used{n.Name + " podCIDR", cidr})
			}
		}
	}
	var svc corev1.Service
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "kubernetes"}, &svc); err == nil {
		for _, ip := range svc.Spec.ClusterIPs {
			uses = append(uses, used{"Service default/kubernetes", ip})
		}
	}
	var clashes []string
	for _, u := range uses {
		p, err := parseAddressOrPrefix(u.value)
		if err != nil {
			continue
		}
		for _, r := range ranges {
			if p.Overlaps(r) {
				clashes = append(clashes, fmt.Sprintf("%s %s overlaps %s", u.what, u.value, r))
			}
		}
	}
	if len(clashes) > 0 {
		return CheckResult{Name: "tunnel-ranges", Pass: false, Detail: strings.Join(clashes, "; ")}
	}
	return CheckResult{Name: "tunnel-ranges", Pass: true, Detail: "disjoint"}
}

func parseAddressOrPrefix(v string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(strings.TrimSpace(v)); err == nil {
		return p, nil
	}
	a, err := netip.ParseAddr(strings.TrimSpace(v))
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func checkEndpointCandidates(ctx context.Context, c client.Reader) CheckResult {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return CheckResult{Name: "endpoint-candidates", Pass: false, Detail: err.Error()}
	}
	var readyLinux []string
	for _, n := range nodes.Items {
		if n.Labels[platform.RoleLabel] == platform.RoleValue {
			continue
		}
		if osLabel := n.Labels["kubernetes.io/os"]; osLabel != "" && osLabel != "linux" {
			continue
		}
		readyLinux = append(readyLinux, n.Name)
	}
	return CheckResult{
		Name:   "endpoint-candidates",
		Pass:   len(readyLinux) > 0,
		Detail: fmt.Sprintf("candidates=%v", readyLinux),
	}
}

// InspectLinuxCalicoDaemonSet validates an unstructured kube-system/calico-node
// DaemonSet for BIRD backend and CNI/Felix MTU alignment.
func InspectLinuxCalicoDaemonSet(u *unstructured.Unstructured, expectedMTU int) error {
	containers, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
	initContainers, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "initContainers")
	bird := false
	felixMTUs := map[string]int{}
	for _, raw := range containers {
		container, ok := raw.(map[string]interface{})
		if !ok || container["name"] != LinuxDaemonSetName {
			continue
		}
		env, _, _ := unstructured.NestedSlice(container, "env")
		for _, value := range env {
			entry, ok := value.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := entry["name"].(string)
			val, _ := entry["value"].(string)
			if name == "CALICO_NETWORKING_BACKEND" && val == "bird" {
				bird = true
			}
			if name == "FELIX_IPINIPMTU" || name == "FELIX_VXLANMTU" || name == "FELIX_WIREGUARDMTU" {
				parsed, err := strconv.Atoi(val)
				if err != nil || parsed < 1280 {
					return fmt.Errorf("stock k0s calico-node has invalid %s=%q", name, val)
				}
				felixMTUs[name] = parsed
			}
		}
	}
	if !bird {
		return fmt.Errorf("stock k0s calico-node must explicitly use the bird backend; refusing unknown/VXLAN configuration")
	}
	var cniMTU int
	for _, raw := range append(initContainers, containers...) {
		container, ok := raw.(map[string]interface{})
		if !ok || container["name"] != "install-cni" {
			continue
		}
		env, _, _ := unstructured.NestedSlice(container, "env")
		for _, value := range env {
			entry, ok := value.(map[string]interface{})
			if !ok || entry["name"] != "CNI_NETWORK_CONFIG" {
				continue
			}
			cfgStr, _ := entry["value"].(string)
			var netCfg struct {
				MTU     int `json:"mtu"`
				Plugins []struct {
					MTU int `json:"mtu"`
				} `json:"plugins"`
			}
			if err := json.Unmarshal([]byte(cfgStr), &netCfg); err != nil {
				return fmt.Errorf("stock k0s calico-node CNI_NETWORK_CONFIG: %w", err)
			}
			cniMTU = netCfg.MTU
			if cniMTU == 0 {
				for _, plugin := range netCfg.Plugins {
					if plugin.MTU != 0 {
						cniMTU = plugin.MTU
						break
					}
				}
			}
		}
	}
	if cniMTU != 0 {
		for _, name := range []string{"FELIX_IPINIPMTU", "FELIX_VXLANMTU", "FELIX_WIREGUARDMTU"} {
			if felixMTU, ok := felixMTUs[name]; ok && felixMTU != cniMTU {
				return fmt.Errorf("stock k0s calico-node CNI_NETWORK_CONFIG mtu (%d) disagrees with %s=%d; set spec.network.calico.mtu=%d in k0s config", cniMTU, name, felixMTU, felixMTU)
			}
		}
		if expectedMTU != 0 && cniMTU != expectedMTU {
			return fmt.Errorf("stock k0s calico-node CNI_NETWORK_CONFIG mtu (%d) does not match Windows BGP MTU (%d)", cniMTU, expectedMTU)
		}
	}
	return nil
}

var optInAWSRegions = map[string]bool{
	"af-south-1":     true,
	"ap-east-1":      true,
	"ap-south-2":     true,
	"ap-southeast-3": true,
	"ap-southeast-4": true,
	"ca-west-1":      true,
	"eu-central-2":   true,
	"eu-south-1":     true,
	"eu-south-2":     true,
	"il-central-1":   true,
	"me-central-1":   true,
	"me-south-1":     true,
}

func isARM64InstanceType(it string) bool {
	family, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(it)), ".")
	if family == "a1" {
		return true
	}
	for _, prefix := range []string{"t4g", "m6g", "m7g", "m8g", "c6g", "c7g", "c8g", "r6g", "r7g", "r8g", "x2gd", "im4gn", "is4gen"} {
		if strings.HasPrefix(family, prefix) {
			return true
		}
	}
	return false
}

func isSmallBurstableInstanceType(it string) bool {
	it = strings.ToLower(strings.TrimSpace(it))
	for _, suffix := range []string{".nano", ".micro", ".small"} {
		if strings.HasSuffix(it, suffix) {
			return true
		}
	}
	return false
}

func checkHelmValues(raw []byte, parsedTuple Tuple) CheckResult {
	var doc map[string]interface{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return CheckResult{Name: "helm-values", Pass: false, Detail: fmt.Sprintf("parse values YAML: %v", err)}
	}
	var warnings []string
	var pinnedFields []string

	for _, arch := range []string{"arm64", "amd64"} {
		urlStr, _, _ := unstructured.NestedString(doc, "dialerBinary", arch, "url")
		shaStr, _, _ := unstructured.NestedString(doc, "dialerBinary", arch, "sha256")
		urlStr = strings.TrimSpace(urlStr)
		shaStr = strings.TrimSpace(shaStr)
		if (urlStr != "") != (shaStr != "") {
			return CheckResult{
				Name:   "helm-values",
				Pass:   false,
				Detail: fmt.Sprintf("dialerBinary.%s.url and dialerBinary.%s.sha256 must both be set when overriding the release manifest", arch, arch),
			}
		}
		if urlStr != "" || shaStr != "" {
			pinnedFields = append(pinnedFields, "dialerBinary."+arch)
		}
	}
	winSHA, _, _ := unstructured.NestedString(doc, "windowsBootstrap", "tunnelSHA256")
	if strings.TrimSpace(winSHA) != "" {
		pinnedFields = append(pinnedFields, "windowsBootstrap.tunnelSHA256")
	}
	if len(pinnedFields) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"hardcoded dialer binary overrides (%s) are brittle; omit them to use dialerBinary.version=\"latest\" with the multi-platform manifest and universal Linux fat binary",
			strings.Join(pinnedFields, ", "),
		))
	}

	if excl, found, _ := unstructured.NestedBool(doc, "siteCNI", "exclusion"); found && !excl {
		if parsedTuple.CloudCNI == "aws-vpc" {
			return CheckResult{
				Name:   "helm-values",
				Pass:   false,
				Detail: "siteCNI.exclusion=false is unsafe when cloudCNI=aws-vpc because site CNI DaemonSets will schedule onto aws-vpc cloud workers",
			}
		}
		warnings = append(warnings, "siteCNI.exclusion=false disables automatic nodeAffinity exclusion on site CNI DaemonSets")
	}

	version, _, _ := unstructured.NestedString(doc, "dialerBinary", "version")
	if strings.TrimSpace(version) == "" {
		version = "latest"
	}
	return CheckResult{
		Name:     "helm-values",
		Pass:     true,
		Detail:   fmt.Sprintf("dialerBinary.version=%s", version),
		Warnings: warnings,
	}
}

func checkManifests(files [][]byte, parsedTuple Tuple) CheckResult {
	var warnings []string
	var count int
	for _, rawFile := range files {
		for _, chunk := range strings.Split(string(rawFile), "\n---") {
			trimmed := strings.TrimSpace(chunk)
			if trimmed == "" || trimmed == "---" {
				continue
			}
			var obj map[string]interface{}
			if err := yaml.Unmarshal([]byte(trimmed), &obj); err != nil {
				return CheckResult{Name: "manifests", Pass: false, Detail: fmt.Sprintf("parse manifest YAML: %v", err)}
			}
			if len(obj) == 0 {
				continue
			}
			u := &unstructured.Unstructured{Object: obj}
			kind := u.GetKind()
			name := u.GetName()
			if kind == "" {
				continue
			}
			count++
			switch kind {
			case "ProvisionedNodeClass":
				osVal, _, _ := unstructured.NestedString(obj, "spec", "os")
				if osVal == "" {
					osVal = "linux"
				}
				cniVal, _, _ := unstructured.NestedString(obj, "spec", "cni")
				itVal, _, _ := unstructured.NestedString(obj, "spec", "instanceType")
				regionVal, _, _ := unstructured.NestedString(obj, "spec", "region")
				amiVal, _, _ := unstructured.NestedString(obj, "spec", "amiID")
				var rootVol int
				if rv, found, _ := unstructured.NestedFieldNoCopy(obj, "spec", "rootVolumeGiB"); found {
					switch v := rv.(type) {
					case int64:
						rootVol = int(v)
					case float64:
						rootVol = int(v)
					case int:
						rootVol = v
					}
				}
				if osVal == "windows" && isARM64InstanceType(itVal) {
					return CheckResult{
						Name:   "manifests",
						Pass:   false,
						Detail: fmt.Sprintf("ProvisionedNodeClass %s pairs os=windows with ARM64 instanceType=%s", name, itVal),
					}
				}
				if osVal == "windows" && !parsedTuple.IncludesWindows() {
					warnings = append(warnings, fmt.Sprintf("ProvisionedNodeClass %s sets os=windows, but selected tuple %s is Linux-only", name, parsedTuple))
				}
				if cniVal != "" && parsedTuple.CloudCNI != "" && cniVal != parsedTuple.CloudCNI {
					warnings = append(warnings, fmt.Sprintf("ProvisionedNodeClass %s sets spec.cni=%s while tuple %s defaults to cloudCNI=%s", name, cniVal, parsedTuple, parsedTuple.CloudCNI))
				}
				if optInAWSRegions[strings.TrimSpace(regionVal)] && strings.TrimSpace(amiVal) == "" {
					warnings = append(warnings, fmt.Sprintf("ProvisionedNodeClass %s targets AWS opt-in region %s without an explicit amiID; ensure the opt-in region is enabled in the AWS account", name, regionVal))
				}
				if isSmallBurstableInstanceType(itVal) && rootVol > 0 && rootVol < 20 {
					warnings = append(warnings, fmt.Sprintf("ProvisionedNodeClass %s uses %s with rootVolumeGiB=%d; recommend >= 20 GiB to avoid disk-pressure evictions", name, itVal, rootVol))
				}
			case "BGPPeer":
				sel, _, _ := unstructured.NestedString(obj, "spec", "nodeSelector")
				if !strings.Contains(sel, platform.RoleLabel) && (sel == "" || strings.Contains(sel, "all()") || strings.Contains(sel, "kubernetes.io/os")) {
					if parsedTuple.CloudCNI == "site" {
						return CheckResult{
							Name:   "manifests",
							Pass:   false,
							Detail: fmt.Sprintf("BGPPeer %s nodeSelector %q selects cloud-worker nodes without '!has(%s)' under cloudCNI=site", name, sel, platform.RoleLabel),
						}
					}
					warnings = append(warnings, fmt.Sprintf("BGPPeer %s nodeSelector %q does not include '!has(%s)'; recommend scoping BGPPeer to on-site nodes", name, sel, platform.RoleLabel))
				}
			case "Deployment":
				var dep appsv1.Deployment
				rawJSON, err := json.Marshal(obj)
				if err == nil && json.Unmarshal(rawJSON, &dep) == nil {
					if w := inspectSingleNodeHostPortDeployment(&dep); w != "" {
						warnings = append(warnings, w)
					}
				}
			case "NginxProxy":
				if w := inspectNginxProxyHostPort(u); w != "" {
					warnings = append(warnings, w)
				}
			case "HelmRelease":
				chartName, _, _ := unstructured.NestedString(u.Object, "spec", "chart", "spec", "chart")
				if chartName == "spegel" || u.GetName() == "spegel" {
					rawSpec, _ := json.Marshal(u.Object["spec"])
					if !strings.Contains(string(rawSpec), platform.RoleValue) {
						warnings = append(warnings, fmt.Sprintf(
							"HelmRelease %s/%s (chart=%s) does not exclude %s=%s in spec.values.affinity or spec.postRenderers; spegel tolerates all taints and will schedule onto remote WAN cloud workers",
							u.GetNamespace(), u.GetName(), chartName, platform.RoleLabel, platform.RoleValue,
						))
					}
				}
			}
		}
	}
	return CheckResult{
		Name:     "manifests",
		Pass:     true,
		Detail:   fmt.Sprintf("inspected %d manifest object(s)", count),
		Warnings: warnings,
	}
}

func checkCloudWorkerWorkloads(ctx context.Context, c client.Reader) (CheckResult, bool) {
	var warnings []string
	var inspectedCount int

	npList := &unstructured.UnstructuredList{}
	npList.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.nginx.org", Version: "v1alpha2", Kind: "NginxProxyList"})
	if err := c.List(ctx, npList); err == nil {
		for i := range npList.Items {
			np := &npList.Items[i]
			hostPorts, found, _ := unstructured.NestedSlice(np.Object, "spec", "kubernetes", "deployment", "container", "hostPorts")
			if !found || len(hostPorts) == 0 {
				continue
			}
			if w := inspectNginxProxyHostPort(np); w != "" {
				warnings = append(warnings, w)
			} else {
				warnings = append(warnings, site.CheckGitOpsWorkloadRecreate(ctx, c, np, "NginxProxy", "/spec/kubernetes/deployment/patches")...)
			}
		}
	}

	var deps appsv1.DeploymentList
	if err := c.List(ctx, &deps); err != nil {
		return CheckResult{}, false
	}
	for i := range deps.Items {
		dep := &deps.Items[i]
		if !targetsCloudWorkersOrSingleNode(dep) {
			continue
		}
		inspectedCount++
		if w := inspectSingleNodeHostPortDeployment(dep); w != "" {
			warnings = append(warnings, w)
		} else if len(dep.OwnerReferences) == 0 && deploymentHasHostPort(dep) {
			warnings = append(warnings, site.CheckGitOpsWorkloadRecreate(ctx, c, dep, "Deployment", "/spec/strategy")...)
		}
	}
	if inspectedCount == 0 && len(warnings) == 0 {
		return CheckResult{}, false
	}
	return CheckResult{
		Name:     "cloud-worker-workloads",
		Pass:     true,
		Detail:   fmt.Sprintf("inspected %d single-node/cloud-worker Deployment(s)", inspectedCount),
		Warnings: warnings,
	}, true
}

func deploymentHasHostPort(dep *appsv1.Deployment) bool {
	for _, container := range dep.Spec.Template.Spec.Containers {
		for _, p := range container.Ports {
			if p.HostPort > 0 {
				return true
			}
		}
	}
	return false
}

func targetsCloudWorkersOrSingleNode(dep *appsv1.Deployment) bool {
	if dep.Spec.Template.Spec.NodeSelector[platform.RoleLabel] == platform.RoleValue {
		return true
	}
	if dep.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != "" {
		return true
	}
	for _, tol := range dep.Spec.Template.Spec.Tolerations {
		if tol.Key == "cloud-provisioning.appmana.com/cloud-worker" || tol.Key == platform.RoleLabel {
			return true
		}
	}
	return false
}

func inspectSingleNodeHostPortDeployment(dep *appsv1.Deployment) string {
	if !targetsCloudWorkersOrSingleNode(dep) {
		return ""
	}
	var hostPorts []int32
	for _, container := range dep.Spec.Template.Spec.Containers {
		for _, p := range container.Ports {
			if p.HostPort > 0 {
				hostPorts = append(hostPorts, p.HostPort)
			}
		}
	}
	if len(hostPorts) == 0 {
		return ""
	}
	if dep.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		return ""
	}
	if dep.Spec.Strategy.RollingUpdate != nil && dep.Spec.Strategy.RollingUpdate.MaxSurge != nil {
		ms := dep.Spec.Strategy.RollingUpdate.MaxSurge.String()
		if ms == "0" || ms == "0%" {
			return ""
		}
	}
	ns := dep.Namespace
	if ns == "" {
		ns = "default"
	}
	target := "cloud-worker"
	if host := dep.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]; host != "" && dep.Spec.Template.Spec.NodeSelector[platform.RoleLabel] == "" {
		target = fmt.Sprintf("single-node (%s)", host)
	}
	return fmt.Sprintf(
		"Deployment %s/%s targets %s with hostPort %v without strategy.type=Recreate or rollingUpdate.maxSurge=0; rollouts will deadlock on port conflicts",
		ns, dep.Name, target, hostPorts,
	)
}

func inspectNginxProxyHostPort(u *unstructured.Unstructured) string {
	if u == nil {
		return ""
	}
	hostPorts, found, _ := unstructured.NestedSlice(u.Object, "spec", "kubernetes", "deployment", "container", "hostPorts")
	if !found || len(hostPorts) == 0 {
		return ""
	}
	patches, _, _ := unstructured.NestedSlice(u.Object, "spec", "kubernetes", "deployment", "patches")
	for _, rawPatch := range patches {
		rawBytes, _ := json.Marshal(rawPatch)
		s := string(rawBytes)
		if strings.Contains(s, "Recreate") || strings.Contains(s, `"maxSurge":0`) || strings.Contains(s, `"maxSurge":"0%"`) {
			return ""
		}
	}
	ns := u.GetNamespace()
	if ns == "" {
		ns = "default"
	}
	return fmt.Sprintf(
		"NginxProxy %s/%s configures deployment.container.hostPorts without a StrategicMerge deployment patch setting strategy.type=Recreate; single-node gateway rollouts will deadlock on port 80/443",
		ns, u.GetName(),
	)
}
