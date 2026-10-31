package tuple

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	FieldManager          = "calico-windows-adapter"
	ownerAnnotation       = "projectcalico.org/windows-adapter-owner"
	WindowsConfigMapName  = "calico-windows-config-actual"
	StockWindowsConfigMap = "calico-windows-config"
	WindowsDaemonSetName  = "calico-node-windows"
	LinuxDaemonSetName    = "calico-node"
)

var digestPattern = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

// WindowsBGPOptions configures the stock k0s Windows BGP resources
// (calico-windows-config-actual, IPAMConfig/default, calico-node-windows).
type WindowsBGPOptions struct {
	NodeImage               string
	APIHost                 string
	APIPort                 string
	ServiceCIDR             string
	DNSAddress              string
	AutodetectionMethod     string
	IPv6AutodetectionMethod string
	MTU                     int
	ExcludeCloudWorkers     bool
	ExcludeNativeCNIs       []string
	Offline                 bool
	PodLabels               map[string]string
	ImagePullSecrets        []string
}

// WindowsBGP renders the caller-owned ConfigMap, IPAMConfig, and DaemonSet
// required for Windows BGP nodes on a stock k0s + Calico BIRD cluster.
func WindowsBGP(o WindowsBGPOptions) ([]runtime.Object, error) {
	if !digestPattern.MatchString(strings.TrimSpace(o.NodeImage)) {
		return nil, fmt.Errorf("node image %q requires an immutable @sha256:<64-hex> digest", o.NodeImage)
	}
	if net.ParseIP(o.APIHost).To4() == nil || net.ParseIP(o.DNSAddress).To4() == nil {
		return nil, fmt.Errorf("explicit IPv4 API and DNS addresses are required, including when IPv6 pod networking is enabled")
	}
	serviceIP, serviceNet, err := net.ParseCIDR(o.ServiceCIDR)
	if err != nil || serviceIP.To4() == nil || !serviceNet.Contains(net.ParseIP(o.DNSAddress)) {
		return nil, fmt.Errorf("an IPv4 service CIDR containing the DNS address is required")
	}
	if o.APIPort != "443" && o.APIPort != "6443" {
		return nil, fmt.Errorf("explicit Kubernetes API port must be 443 or 6443")
	}
	if strings.TrimSpace(o.AutodetectionMethod) == "" || strings.TrimSpace(o.AutodetectionMethod) == "first-found" {
		return nil, fmt.Errorf("explicit non-first-found address autodetection method is required")
	}
	if o.IPv6AutodetectionMethod != "" && (strings.TrimSpace(o.IPv6AutodetectionMethod) == "" || strings.TrimSpace(o.IPv6AutodetectionMethod) == "first-found") {
		return nil, fmt.Errorf("IPv6 address autodetection method cannot be whitespace or first-found")
	}
	if o.MTU != 0 && (o.MTU < 1280 || o.MTU > 9000) {
		return nil, fmt.Errorf("explicit MTU %d must be between 1280 and 9000", o.MTU)
	}
	var nativeCNIs []string
	seenCNIs := map[string]bool{}
	for _, cniName := range o.ExcludeNativeCNIs {
		if cniName == "" || len(validation.IsValidLabelValue(cniName)) != 0 || seenCNIs[cniName] {
			return nil, fmt.Errorf("invalid or repeated native CNI exclusion %q", cniName)
		}
		seenCNIs[cniName] = true
		nativeCNIs = append(nativeCNIs, cniName)
	}

	cm := &v1.ConfigMap{
		TypeMeta:   meta.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: meta.ObjectMeta{Name: WindowsConfigMapName, Namespace: "kube-system"},
		Data: map[string]string{
			"CALICO_NETWORKING_BACKEND": "windows-bgp",
			"DATASTORE_TYPE":            "kubernetes",
			"KUBERNETES_SERVICE_HOST":   o.APIHost,
			"KUBERNETES_SERVICE_PORT":   o.APIPort,
			"K8S_SERVICE_CIDR":          o.ServiceCIDR,
			"DNS_NAME_SERVERS":          o.DNSAddress,
			"IP":                        "autodetect",
			"IP_AUTODETECTION_METHOD":   o.AutodetectionMethod,
			"IP6":                       "none",
			"FELIX_IPV6SUPPORT":         "false",
			"FELIX_HEALTHENABLED":       "true",
			"CALICO_DSR_DISABLE":        "true",
			"CNI_BIN_DIR":               `c:\opt\cni\bin`,
			"CNI_CONF_DIR":              `c:\etc\cni\net.d`,
			"KUBECONFIG":                `c:\etc\cni\net.d\calico-kubeconfig`,
		},
	}
	if o.IPv6AutodetectionMethod != "" {
		cm.Data["IP6"] = "autodetect"
		cm.Data["FELIX_IPV6SUPPORT"] = "true"
		cm.Data["IP6_AUTODETECTION_METHOD"] = o.IPv6AutodetectionMethod
	}
	if o.MTU != 0 {
		mtuStr := strconv.Itoa(o.MTU)
		cm.Data["CNI_MTU"] = mtuStr
		cm.Data["FELIX_IPINIPMTU"] = mtuStr
		cm.Data["FELIX_VXLANMTU"] = mtuStr
		cm.Data["FELIX_WIREGUARDMTU"] = mtuStr
	}

	labels := map[string]string{"k8s-app": WindowsDaemonSetName}
	podLabels := map[string]string{"k8s-app": WindowsDaemonSetName}
	for key, value := range o.PodLabels {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return nil, fmt.Errorf("invalid pod label %q=%q", key, value)
		}
		if key == "k8s-app" && value != labels[key] {
			return nil, fmt.Errorf("pod labels cannot override the DaemonSet selector")
		}
		podLabels[key] = value
	}
	var pullSecrets []v1.LocalObjectReference
	seen := map[string]bool{}
	for _, name := range o.ImagePullSecrets {
		if len(validation.IsDNS1123Subdomain(name)) != 0 || seen[name] {
			return nil, fmt.Errorf("invalid or repeated image-pull secret %q", name)
		}
		seen[name] = true
		pullSecrets = append(pullSecrets, v1.LocalObjectReference{Name: name})
	}

	var affinity *v1.Affinity
	if o.ExcludeCloudWorkers || len(nativeCNIs) > 0 {
		var exprs []v1.NodeSelectorRequirement
		if o.ExcludeCloudWorkers {
			exprs = append(exprs, v1.NodeSelectorRequirement{
				Key:      platform.RoleLabel,
				Operator: v1.NodeSelectorOpNotIn,
				Values:   []string{platform.RoleValue},
			})
		}
		if len(nativeCNIs) > 0 {
			exprs = append(exprs, v1.NodeSelectorRequirement{
				Key:      platform.CNILabel,
				Operator: v1.NodeSelectorOpNotIn,
				Values:   nativeCNIs,
			})
		}
		affinity = &v1.Affinity{
			NodeAffinity: &v1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
					NodeSelectorTerms: []v1.NodeSelectorTerm{{MatchExpressions: exprs}},
				},
			},
		}
	}

	maxUnavailable := intstr.FromInt32(1)
	hostProcess, system := true, `NT AUTHORITY\SYSTEM`
	ds := &apps.DaemonSet{
		TypeMeta:   meta.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: meta.ObjectMeta{Name: WindowsDaemonSetName, Namespace: "kube-system"},
		Spec: apps.DaemonSetSpec{
			Selector:       &meta.LabelSelector{MatchLabels: labels},
			UpdateStrategy: apps.DaemonSetUpdateStrategy{Type: apps.RollingUpdateDaemonSetStrategyType, RollingUpdate: &apps.RollingUpdateDaemonSet{MaxUnavailable: &maxUnavailable}},
			Template: v1.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{Labels: podLabels},
				Spec: v1.PodSpec{
					Affinity:           affinity,
					ImagePullSecrets:   pullSecrets,
					ServiceAccountName: LinuxDaemonSetName,
					HostNetwork:        true,
					NodeSelector:       map[string]string{"kubernetes.io/os": "windows"},
					SecurityContext:    &v1.PodSecurityContext{WindowsOptions: &v1.WindowsSecurityContextOptions{HostProcess: &hostProcess, RunAsUserName: &system}},
					Tolerations:        []v1.Toleration{{Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoSchedule}, {Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoExecute}},
					Volumes:            []v1.Volume{{Name: "host-root", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/"}}}},
				},
			},
		},
	}

	const root = "$env:CONTAINER_SANDBOX_MOUNT_POINT/CalicoWindows"
	pullPolicy := v1.PullIfNotPresent
	if o.Offline {
		pullPolicy = v1.PullNever
	}
	for _, entry := range []struct{ name, script string }{{"node", "/node-service.ps1"}, {"felix", "/felix-service.ps1"}, {"confd", "/confd/confd-service.ps1"}} {
		container := v1.Container{
			Name:            entry.name,
			Image:           o.NodeImage,
			ImagePullPolicy: pullPolicy,
			Args:            []string{root + entry.script},
			WorkingDir:      root,
			VolumeMounts:    []v1.VolumeMount{{Name: "host-root", MountPath: "/host"}},
			EnvFrom:         []v1.EnvFromSource{{ConfigMapRef: &v1.ConfigMapEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: WindowsConfigMapName}}}},
			Env: []v1.EnvVar{
				{Name: "NODENAME", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
				{Name: "NODE_IP", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
			},
		}
		if entry.name == "node" {
			container.ReadinessProbe = &v1.Probe{
				ProbeHandler: v1.ProbeHandler{Exec: &v1.ExecAction{Command: []string{
					"powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
					`$ErrorActionPreference='Stop'; $root=Join-Path $env:CONTAINER_SANDBOX_MOUNT_POINT 'CalicoWindows'; Import-Module (Join-Path $root 'libs/hns/hns.psm1') -Force -DisableNameChecking; Import-Module (Join-Path $root 'libs/calico/calico.psm1') -Force; if (-not (Test-CalicoBGPNodeReady)) { exit 1 }`,
				}}},
				PeriodSeconds:    10,
				TimeoutSeconds:   10,
				FailureThreshold: 1,
			}
		}
		if entry.name == "felix" {
			container.ReadinessProbe = &v1.Probe{
				ProbeHandler:   v1.ProbeHandler{Exec: &v1.ExecAction{Command: []string{root + "/calico-node.exe", "-felix-ready"}}},
				PeriodSeconds:  10,
				TimeoutSeconds: 10,
			}
			container.LivenessProbe = &v1.Probe{
				ProbeHandler:        v1.ProbeHandler{Exec: &v1.ExecAction{Command: []string{root + "/calico-node.exe", "-felix-live"}}},
				InitialDelaySeconds: 10,
				PeriodSeconds:       10,
				TimeoutSeconds:      10,
				FailureThreshold:    6,
			}
			container.StartupProbe = container.LivenessProbe.DeepCopy()
			container.StartupProbe.InitialDelaySeconds = 0
			container.StartupProbe.FailureThreshold = 60
			container.Lifecycle = &v1.Lifecycle{PreStop: &v1.LifecycleHandler{Exec: &v1.ExecAction{Command: []string{root + "/calico-node.exe", "-shutdown"}}}}
		}
		ds.Spec.Template.Spec.Containers = append(ds.Spec.Template.Spec.Containers, container)
	}
	ipam := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "crd.projectcalico.org/v1",
		"kind":       "IPAMConfig",
		"metadata":   map[string]interface{}{"name": "default"},
		"spec":       map[string]interface{}{"strictAffinity": true, "autoAllocateBlocks": true},
	}}
	return []runtime.Object{cm, ipam, ds}, nil
}

// Plan wraps a deterministic, reviewable Windows BGP manifest List.
type Plan struct {
	objects  []*unstructured.Unstructured
	manifest []byte
}

func NewPlan(o WindowsBGPOptions) (*Plan, error) {
	objects, err := WindowsBGP(o)
	if err != nil {
		return nil, err
	}
	shared := []bool{false, true, false}
	p := &Plan{}
	for i, object := range objects {
		data, err := json.Marshal(object)
		if err != nil {
			return nil, err
		}
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(data); err != nil {
			return nil, err
		}
		delete(u.Object, "status")
		if !shared[i] {
			u.SetAnnotations(map[string]string{ownerAnnotation: FieldManager})
		}
		p.objects = append(p.objects, u)
	}
	p.manifest, err = json.Marshal(struct {
		APIVersion string                       `json:"apiVersion"`
		Kind       string                       `json:"kind"`
		Items      []*unstructured.Unstructured `json:"items"`
	}{"v1", "List", p.objects})
	return p, err
}

func (p *Plan) Manifest() []byte                        { return append([]byte(nil), p.manifest...) }
func (p *Plan) SHA256() string                          { return fmt.Sprintf("%x", sha256.Sum256(p.manifest)) }
func (p *Plan) Objects() []*unstructured.Unstructured   { return p.objects }

// K0sNetworkOptions configures the k0s ClusterConfig spec.controllerManager and
// spec.network fragment required for Calico BGP with Windows and cloud-provisioning.
type K0sNetworkOptions struct {
	MTU                     int
	AutodetectionMethod     string
	IPv6AutodetectionMethod string
	PodCIDR                 string
	ServiceCIDR             string
	IPv6PodCIDR             string
	IPv6ServiceCIDR         string
}

type K0sControllerManagerSpec struct {
	ExtraArgs map[string]string `json:"extraArgs"`
}

type K0sCalicoSpec struct {
	Mode    string            `json:"mode"`
	Overlay string            `json:"overlay"`
	MTU     int               `json:"mtu"`
	EnvVars map[string]string `json:"envVars"`
}

type K0sDualStackSpec struct {
	Enabled         bool   `json:"enabled"`
	IPv6PodCIDR     string `json:"ipv6PodCIDR,omitempty"`
	IPv6ServiceCIDR string `json:"ipv6ServiceCIDR,omitempty"`
}

type K0sNetworkSpec struct {
	Provider    string            `json:"provider"`
	PodCIDR     string            `json:"podCIDR,omitempty"`
	ServiceCIDR string            `json:"serviceCIDR,omitempty"`
	DualStack   *K0sDualStackSpec `json:"dualStack,omitempty"`
	Calico      K0sCalicoSpec     `json:"calico"`
}

type K0sNetworkInputs struct {
	ControllerManager K0sControllerManagerSpec `json:"controllerManager"`
	Network           K0sNetworkSpec           `json:"network"`
}

// K0sCalicoNetwork renders the k0s spec.controllerManager + spec.network fragment
// that sets allocate-node-cidrs=false and aligns spec.network.calico.mtu (which
// k0s writes into 10-calico.conflist) with FELIX_IPINIPMTU/FELIX_VXLANMTU/FELIX_WIREGUARDMTU.
func K0sCalicoNetwork(o K0sNetworkOptions) (K0sNetworkInputs, error) {
	if o.MTU < 1280 || o.MTU > 9000 {
		return K0sNetworkInputs{}, fmt.Errorf("explicit Calico MTU %d must be between 1280 and 9000 (k0s defaults 10-calico.conflist to 1450 if omitted)", o.MTU)
	}
	method := strings.TrimSpace(o.AutodetectionMethod)
	if method == "" || method == "first-found" {
		return K0sNetworkInputs{}, fmt.Errorf("explicit non-first-found IP_AUTODETECTION_METHOD is required")
	}
	if o.PodCIDR != "" {
		if ip, _, err := net.ParseCIDR(o.PodCIDR); err != nil || ip.To4() == nil {
			return K0sNetworkInputs{}, fmt.Errorf("invalid IPv4 podCIDR %q", o.PodCIDR)
		}
	}
	if o.ServiceCIDR != "" {
		if ip, _, err := net.ParseCIDR(o.ServiceCIDR); err != nil || ip.To4() == nil {
			return K0sNetworkInputs{}, fmt.Errorf("invalid IPv4 serviceCIDR %q", o.ServiceCIDR)
		}
	}
	mtuStr := strconv.Itoa(o.MTU)
	env := map[string]string{
		"IP_AUTODETECTION_METHOD": o.AutodetectionMethod,
		"FELIX_IPINIPMTU":         mtuStr,
		"FELIX_VXLANMTU":          mtuStr,
		"FELIX_WIREGUARDMTU":      mtuStr,
	}
	var dualStack *K0sDualStackSpec
	if o.IPv6AutodetectionMethod != "" || o.IPv6PodCIDR != "" || o.IPv6ServiceCIDR != "" {
		v6Method := strings.TrimSpace(o.IPv6AutodetectionMethod)
		if v6Method == "" || v6Method == "first-found" {
			return K0sNetworkInputs{}, fmt.Errorf("explicit non-first-found IP6_AUTODETECTION_METHOD is required when IPv6 is enabled")
		}
		if o.IPv6PodCIDR != "" {
			if ip, _, err := net.ParseCIDR(o.IPv6PodCIDR); err != nil || ip.To4() != nil {
				return K0sNetworkInputs{}, fmt.Errorf("invalid IPv6 podCIDR %q", o.IPv6PodCIDR)
			}
		}
		if o.IPv6ServiceCIDR != "" {
			if ip, _, err := net.ParseCIDR(o.IPv6ServiceCIDR); err != nil || ip.To4() != nil {
				return K0sNetworkInputs{}, fmt.Errorf("invalid IPv6 serviceCIDR %q", o.IPv6ServiceCIDR)
			}
		}
		env["IP6"] = "autodetect"
		env["FELIX_IPV6SUPPORT"] = "true"
		env["IP6_AUTODETECTION_METHOD"] = o.IPv6AutodetectionMethod
		dualStack = &K0sDualStackSpec{
			Enabled:         true,
			IPv6PodCIDR:     o.IPv6PodCIDR,
			IPv6ServiceCIDR: o.IPv6ServiceCIDR,
		}
	}
	return K0sNetworkInputs{
		ControllerManager: K0sControllerManagerSpec{
			ExtraArgs: map[string]string{"allocate-node-cidrs": "false"},
		},
		Network: K0sNetworkSpec{
			Provider:    "calico",
			PodCIDR:     o.PodCIDR,
			ServiceCIDR: o.ServiceCIDR,
			DualStack:   dualStack,
			Calico: K0sCalicoSpec{
				Mode:    "bird",
				Overlay: "Never",
				MTU:     o.MTU,
				EnvVars: env,
			},
		},
	}, nil
}

// ValidateK0sCalicoConfig inspects a k0s.yaml or k0sctl.yaml document and rejects
// configurations that omit allocate-node-cidrs=false, run non-BGP Calico, omit
// explicit IP autodetection, or let spec.network.calico.mtu drift from FELIX_*MTU.
func ValidateK0sCalicoConfig(raw []byte) error {
	var doc map[string]interface{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse k0s config: %w", err)
	}
	spec, ok, _ := unstructured.NestedMap(doc, "spec", "k0s", "config", "spec")
	if !ok {
		spec, ok, _ = unstructured.NestedMap(doc, "spec")
	}
	if !ok || len(spec) == 0 {
		return fmt.Errorf("missing k0s spec (expected spec or spec.k0s.config.spec)")
	}
	alloc, _, _ := unstructured.NestedString(spec, "controllerManager", "extraArgs", "allocate-node-cidrs")
	if alloc != "false" {
		return fmt.Errorf("spec.controllerManager.extraArgs.allocate-node-cidrs must be \"false\" when using Calico IPAM (got %q)", alloc)
	}
	provider, _, _ := unstructured.NestedString(spec, "network", "provider")
	if provider != "calico" {
		return fmt.Errorf("spec.network.provider must be \"calico\" (got %q)", provider)
	}
	mode, _, _ := unstructured.NestedString(spec, "network", "calico", "mode")
	if mode != "bird" {
		return fmt.Errorf("spec.network.calico.mode must be \"bird\" for BGP (got %q)", mode)
	}
	overlay, _, _ := unstructured.NestedString(spec, "network", "calico", "overlay")
	if overlay != "Never" {
		return fmt.Errorf("spec.network.calico.overlay must be \"Never\" for unencapsulated BGP (got %q)", overlay)
	}
	var cniMTU int
	if mtuVal, found, _ := unstructured.NestedFieldNoCopy(spec, "network", "calico", "mtu"); found {
		switch v := mtuVal.(type) {
		case int64:
			cniMTU = int(v)
		case float64:
			cniMTU = int(v)
		case int:
			cniMTU = v
		}
	}
	if cniMTU < 1280 || cniMTU > 9000 {
		return fmt.Errorf("spec.network.calico.mtu must be explicitly set between 1280 and 9000 (got %d; omitting it leaves 10-calico.conflist at k0s default 1450)", cniMTU)
	}
	envVars, _, _ := unstructured.NestedStringMap(spec, "network", "calico", "envVars")
	autodetect := strings.TrimSpace(envVars["IP_AUTODETECTION_METHOD"])
	if autodetect == "" || autodetect == "first-found" {
		return fmt.Errorf("spec.network.calico.envVars.IP_AUTODETECTION_METHOD must be explicitly set and not first-found (got %q)", autodetect)
	}
	dualEnabled, _, _ := unstructured.NestedBool(spec, "network", "dualStack", "enabled")
	if dualEnabled {
		v6Autodetect := strings.TrimSpace(envVars["IP6_AUTODETECTION_METHOD"])
		if v6Autodetect == "" || v6Autodetect == "first-found" {
			return fmt.Errorf("spec.network.calico.envVars.IP6_AUTODETECTION_METHOD must be explicitly set when dualStack is enabled (got %q)", v6Autodetect)
		}
	}
	for _, key := range []string{"FELIX_IPINIPMTU", "FELIX_VXLANMTU", "FELIX_WIREGUARDMTU"} {
		val, exists := envVars[key]
		if !exists {
			continue
		}
		parsed, err := strconv.Atoi(val)
		if err != nil || parsed != cniMTU {
			return fmt.Errorf("spec.network.calico.envVars.%s=%q does not match spec.network.calico.mtu=%d", key, val, cniMTU)
		}
	}
	return nil
}

// InspectK0sCalicoConfig runs ValidateK0sCalicoConfig and also returns
// environment warnings for the selected platform tuple and WireGuard tunnel MTU.
func InspectK0sCalicoConfig(raw []byte, parsedTuple Tuple, tunnelMTU int) ([]string, error) {
	if err := ValidateK0sCalicoConfig(raw); err != nil {
		return nil, err
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse k0s config: %w", err)
	}
	spec, ok, _ := unstructured.NestedMap(doc, "spec", "k0s", "config", "spec")
	if !ok {
		spec, _, _ = unstructured.NestedMap(doc, "spec")
	}
	var cniMTU int
	if mtuVal, found, _ := unstructured.NestedFieldNoCopy(spec, "network", "calico", "mtu"); found {
		switch v := mtuVal.(type) {
		case int64:
			cniMTU = int(v)
		case float64:
			cniMTU = int(v)
		case int:
			cniMTU = v
		}
	}
	if tunnelMTU <= 0 {
		tunnelMTU = 1420
	}
	if cniMTU > tunnelMTU && parsedTuple.CloudCNI == "site" {
		return nil, fmt.Errorf("spec.network.calico.mtu (%d) exceeds WireGuard tunnel MTU (%d) for cloudCNI=site", cniMTU, tunnelMTU)
	}
	var warnings []string
	if cniMTU > tunnelMTU && parsedTuple.CloudCNI == "aws-vpc" {
		warnings = append(warnings, fmt.Sprintf(
			"spec.network.calico.mtu=%d exceeds WireGuard tunnel MTU=%d; aws-node clamps aws-vpc cloud-worker pods to %d while site pods use %d",
			cniMTU, tunnelMTU, tunnelMTU, cniMTU,
		))
	}
	rawStr := string(raw)
	if strings.Contains(rawStr, "vip0") && strings.Contains(rawStr, "/32") {
		warnings = append(warnings, "k0sctl hooks configure a /32 VIP on vip0; ensure vip0 is outside the IP_AUTODETECTION_METHOD CIDR or excluded via can-reach/interface rules so Calico BIRD never binds to vip0")
	}
	if strings.Contains(rawStr, "tailscale0") || strings.Contains(rawStr, "mtu 4200") || strings.Contains(rawStr, "roce") {
		warnings = append(warnings, "k0sctl config references secondary host fabrics (RoCE jumbo MTU / tailscale0); keep IP_AUTODETECTION_METHOD and IP6_AUTODETECTION_METHOD pinned to the primary site LAN CIDR")
	}
	return warnings, nil
}
