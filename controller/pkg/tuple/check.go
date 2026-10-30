package tuple

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
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
)

// CheckResult is one named outcome of a platform tuple health/environment check.
type CheckResult struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// CheckOptions configures Evaluate for a target platform tuple and cluster.
type CheckOptions struct {
	// Tuple is the platform tuple string, e.g. "k0s + calico,bgp + aws".
	Tuple string
	// K0sConfig is optional raw k0s.yaml or k0sctl.yaml bytes to validate.
	K0sConfig []byte
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
}

// Passed reports whether every CheckResult succeeded.
func Passed(results []CheckResult) bool {
	for _, r := range results {
		if !r.Pass {
			return false
		}
	}
	return len(results) > 0
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
		return out, nil
	}
	out = append(out, CheckResult{
		Name:   "tuple",
		Pass:   true,
		Detail: fmt.Sprintf("%s (os=%s cloudCNI=%s)", parsedTuple, parsedTuple.GuestOS, parsedTuple.CloudCNI),
	})

	if len(opts.K0sConfig) > 0 {
		if err := ValidateK0sCalicoConfig(opts.K0sConfig); err != nil {
			out = append(out, CheckResult{Name: "k0s-config", Pass: false, Detail: err.Error()})
		} else {
			out = append(out, CheckResult{Name: "k0s-config", Pass: true, Detail: "allocate-node-cidrs=false, calico bird/Never, and MTU aligned with FELIX_*MTU"})
		}
	}

	if c == nil {
		return out, nil
	}

	network, err := cni.Detect(ctx, c)
	if err != nil {
		out = append(out, CheckResult{Name: "network", Pass: false, Detail: err.Error()})
		return out, nil
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

	if parsedTuple.CNI == cni.Calico {
		out = append(out, checkCalicoMTU(ctx, c, parsedTuple, opts.TunnelMTU))
		out = append(out, checkCalicoAutodetection(ctx, c, opts.TunnelIPv4, opts.TunnelIPv6, dualStack || opts.RequireDualStack))
	}

	windowsPresent, err := hasWindowsNodeOrDaemonSet(ctx, c)
	if err != nil {
		return nil, err
	}
	if parsedTuple.CNI == cni.Calico && (opts.RequireWindowsBGP || (parsedTuple.IncludesWindows() && windowsPresent)) {
		out = append(out, checkWindowsBGP(ctx, c, dualStack || opts.RequireDualStack))
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

	return out, nil
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

func checkCalicoMTU(ctx context.Context, c client.Reader, parsedTuple Tuple, tunnelMTU int) CheckResult {
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
	return CheckResult{
		Name:   "calico-mtu",
		Pass:   true,
		Detail: fmt.Sprintf("calico CNI and Felix MTU aligned at %d (%s, cloudCNI=%s tunnelMTU=%d)", cniMTU, source, parsedTuple.CloudCNI, tunnelMTU),
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
	if hasLinuxExclusion && hasWindowsExclusion {
		return CheckResult{Name: "site-cni-exclusion", Pass: true, Detail: "site CNI DaemonSets exclude cloud-worker and native CNI nodes"}
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
	return CheckResult{Name: "site-cni-exclusion", Pass: true, Detail: "no cloud-worker nodes present yet; endpoint-controller Excluder will apply nodeAffinity"}
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
