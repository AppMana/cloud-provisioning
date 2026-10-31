package site

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	cniConfigMTUPattern = regexp.MustCompile(`("mtu"\s*:\s*)(\d+)`)
	caliVethMTUPattern  = regexp.MustCompile(`(?m)^\d+:\s+(cali[a-zA-Z0-9_-]+)(?:@[^:\s]+)?:\s+<[^>]*>\s+mtu\s+(\d+)\b`)
)

// ParseCaliVethMTUs parses `ip -o link show` output and returns a map of
// Calico host veth interface names (cali*) to their current MTU.
func ParseCaliVethMTUs(ipLinkOutput string) map[string]int {
	out := map[string]int{}
	for _, m := range caliVethMTUPattern.FindAllStringSubmatch(ipLinkOutput, -1) {
		if len(m) != 3 {
			continue
		}
		if mtu, err := strconv.Atoi(m[2]); err == nil && mtu > 0 {
			out[m[1]] = mtu
		}
	}
	return out
}

// ReconcileReport summarizes changes applied (or planned) by ReconcileCalicoSite.
type ReconcileReport struct {
	EffectiveMTU          int      `json:"effectiveMTU,omitempty"`
	LinuxDaemonSetPatched bool     `json:"linuxDaemonSetPatched,omitempty"`
	WindowsConfigPatched  bool     `json:"windowsConfigPatched,omitempty"`
	WindowsDaemonPatched  bool     `json:"windowsDaemonSetPatched,omitempty"`
	IPAMConfigPatched     bool     `json:"ipamConfigPatched,omitempty"`
	ExclusionsPatched     bool     `json:"exclusionsPatched,omitempty"`
	WorkloadsPatched      bool     `json:"workloadsPatched,omitempty"`
	GitOpsPatched         bool     `json:"gitOpsPatched,omitempty"`
	ManifestFilesPatched  bool     `json:"manifestFilesPatched,omitempty"`
	VethsClamped          int      `json:"vethsClamped,omitempty"`
	Changes               []string `json:"changes,omitempty"`
}

// AlignLinuxCalicoMTU inspects a Linux calico-node DaemonSet and ensures its
// install-cni CNI_NETWORK_CONFIG "mtu" (and CNI_MTU if present) and calico-node
// FELIX_IPINIPMTU / FELIX_VXLANMTU / FELIX_WIREGUARDMTU agree.
//
// When targetMTU > 0, both CNI and Felix MTUs are set to targetMTU.
// When targetMTU == 0 and CNI_NETWORK_CONFIG "mtu" disagrees with FELIX_*MTU
// (for example, k0s's default 1450 in CNI_NETWORK_CONFIG when FELIX_IPINIPMTU=1500
// was set via envVars), it aligns them to the explicit Felix MTU (or CNI MTU if
// CNI_NETWORK_CONFIG was explicitly changed from 1450).
func AlignLinuxCalicoMTU(ds *appsv1.DaemonSet, targetMTU int) (changed bool, effectiveMTU int, changes []string, err error) {
	if ds == nil {
		return false, 0, nil, nil
	}
	var cniMTU int
	felixMTUs := map[string]int{}
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name != calicoAgent {
			continue
		}
		for _, e := range c.Env {
			switch e.Name {
			case "FELIX_IPINIPMTU", "FELIX_VXLANMTU", "FELIX_WIREGUARDMTU":
				if v, convErr := strconv.Atoi(strings.TrimSpace(e.Value)); convErr == nil && v >= 1280 {
					felixMTUs[e.Name] = v
				}
			}
		}
	}
	for _, c := range ds.Spec.Template.Spec.InitContainers {
		if c.Name != "install-cni" {
			continue
		}
		for _, e := range c.Env {
			if e.Name != "CNI_NETWORK_CONFIG" || e.Value == "" {
				continue
			}
			var netCfg struct {
				MTU     int `json:"mtu"`
				Plugins []struct {
					Type string          `json:"type"`
					MTU  json.RawMessage `json:"mtu"`
				} `json:"plugins"`
			}
			if jsonErr := json.Unmarshal([]byte(e.Value), &netCfg); jsonErr != nil {
				return false, 0, nil, fmt.Errorf("parsing install-cni CNI_NETWORK_CONFIG: %w", jsonErr)
			}
			if netCfg.MTU >= 1280 {
				cniMTU = netCfg.MTU
			}
			for _, p := range netCfg.Plugins {
				var literal int
				if json.Unmarshal(p.MTU, &literal) == nil && literal >= 1280 {
					cniMTU = literal
					break
				}
			}
		}
	}

	desired := targetMTU
	if desired == 0 {
		for _, name := range []string{"FELIX_IPINIPMTU", "FELIX_VXLANMTU", "FELIX_WIREGUARDMTU"} {
			if v := felixMTUs[name]; v >= 1280 {
				desired = v
				break
			}
		}
		if cniMTU >= 1280 && cniMTU != 1450 && desired == 1450 {
			desired = cniMTU
		}
		if desired == 0 {
			desired = cniMTU
		}
	}
	if desired < 1280 {
		return false, 0, nil, nil
	}
	effectiveMTU = desired
	desiredStr := strconv.Itoa(desired)

	for i := range ds.Spec.Template.Spec.InitContainers {
		c := &ds.Spec.Template.Spec.InitContainers[i]
		if c.Name != "install-cni" {
			continue
		}
		for j := range c.Env {
			switch c.Env[j].Name {
			case "CNI_NETWORK_CONFIG":
				if cniMTU != 0 && cniMTU != desired {
					replaced := cniConfigMTUPattern.ReplaceAllString(c.Env[j].Value, "${1}"+desiredStr)
					if replaced != c.Env[j].Value {
						c.Env[j].Value = replaced
						changed = true
						changes = append(changes, fmt.Sprintf("calico-node install-cni CNI_NETWORK_CONFIG mtu %d -> %d", cniMTU, desired))
					}
				}
			case "CNI_MTU":
				if c.Env[j].ValueFrom == nil && strings.TrimSpace(c.Env[j].Value) != desiredStr {
					old := c.Env[j].Value
					c.Env[j].Value = desiredStr
					changed = true
					changes = append(changes, fmt.Sprintf("calico-node install-cni CNI_MTU %q -> %q", old, desiredStr))
				}
			}
		}
	}

	for i := range ds.Spec.Template.Spec.Containers {
		c := &ds.Spec.Template.Spec.Containers[i]
		if c.Name != calicoAgent {
			continue
		}
		for j := range c.Env {
			switch c.Env[j].Name {
			case "FELIX_IPINIPMTU", "FELIX_VXLANMTU", "FELIX_WIREGUARDMTU", "CNI_MTU":
				if c.Env[j].ValueFrom == nil && strings.TrimSpace(c.Env[j].Value) != desiredStr {
					old := c.Env[j].Value
					c.Env[j].Value = desiredStr
					changed = true
					changes = append(changes, fmt.Sprintf("calico-node %s %q -> %q", c.Env[j].Name, old, desiredStr))
				}
			}
		}
	}

	return changed, effectiveMTU, changes, nil
}

// AlignWindowsBGPConfigMap reconciles kube-system/calico-windows-config-actual
// so Windows BGP nodes have explicit IP="autodetect" (and IP6="autodetect" when
// dual-stack), CALICO_DSR_DISABLE="true", and CNI_MTU / FELIX_MTU aligned with
// the cluster's effective Calico MTU.
func AlignWindowsBGPConfigMap(cm *corev1.ConfigMap, effectiveMTU int) (changed bool, changes []string) {
	if cm == nil {
		return false, nil
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	if strings.TrimSpace(cm.Data["CALICO_NETWORKING_BACKEND"]) != "windows-bgp" {
		return false, nil
	}
	if strings.TrimSpace(cm.Data["IP_AUTODETECTION_METHOD"]) != "" && strings.TrimSpace(cm.Data["IP"]) != "autodetect" {
		old := cm.Data["IP"]
		cm.Data["IP"] = "autodetect"
		changed = true
		changes = append(changes, fmt.Sprintf("%s IP %q -> \"autodetect\"", cm.Name, old))
	}
	if strings.TrimSpace(cm.Data["FELIX_IPV6SUPPORT"]) == "true" &&
		strings.TrimSpace(cm.Data["IP6_AUTODETECTION_METHOD"]) != "" &&
		strings.TrimSpace(cm.Data["IP6"]) != "autodetect" {
		old := cm.Data["IP6"]
		cm.Data["IP6"] = "autodetect"
		changed = true
		changes = append(changes, fmt.Sprintf("%s IP6 %q -> \"autodetect\"", cm.Name, old))
	}
	if strings.TrimSpace(cm.Data["CALICO_DSR_DISABLE"]) != "true" {
		old := cm.Data["CALICO_DSR_DISABLE"]
		cm.Data["CALICO_DSR_DISABLE"] = "true"
		changed = true
		changes = append(changes, fmt.Sprintf("%s CALICO_DSR_DISABLE %q -> \"true\"", cm.Name, old))
	}
	if effectiveMTU >= 1280 {
		mtuStr := strconv.Itoa(effectiveMTU)
		for _, key := range []string{"CNI_MTU", "FELIX_MTU"} {
			if strings.TrimSpace(cm.Data[key]) != mtuStr {
				old := cm.Data[key]
				cm.Data[key] = mtuStr
				changed = true
				changes = append(changes, fmt.Sprintf("%s %s %q -> %q", cm.Name, key, old, mtuStr))
			}
		}
	}
	return changed, changes
}

// AlignWindowsCalicoDaemonSetMTU aligns literal CNI_MTU and FELIX_*MTU env vars
// on kube-system/calico-node-windows when present.
func AlignWindowsCalicoDaemonSetMTU(ds *appsv1.DaemonSet, effectiveMTU int) (changed bool, changes []string) {
	if ds == nil || effectiveMTU < 1280 {
		return false, nil
	}
	mtuStr := strconv.Itoa(effectiveMTU)
	update := func(containers []corev1.Container) {
		for i := range containers {
			c := &containers[i]
			for j := range c.Env {
				switch c.Env[j].Name {
				case "CNI_MTU", "FELIX_MTU", "FELIX_VXLANMTU", "FELIX_IPINIPMTU", "FELIX_WIREGUARDMTU":
					if c.Env[j].ValueFrom == nil && strings.TrimSpace(c.Env[j].Value) != mtuStr {
						old := c.Env[j].Value
						c.Env[j].Value = mtuStr
						changed = true
						changes = append(changes, fmt.Sprintf("calico-node-windows/%s %s %q -> %q", c.Name, c.Env[j].Name, old, mtuStr))
					}
				}
			}
		}
	}
	update(ds.Spec.Template.Spec.InitContainers)
	update(ds.Spec.Template.Spec.Containers)
	return changed, changes
}

// ReconcileCalicoSite ensures that the cluster's Linux calico-node DaemonSet,
// Windows BGP ConfigMap (calico-windows-config-actual), Windows Calico
// DaemonSet (calico-node-windows), IPAMConfig/default, wildcard-toleration
// DaemonSets (such as spegel), and single-node hostPort workloads have aligned
// MTU, BGP IPAM strictAffinity, and cloud-worker exclusions.
func ReconcileCalicoSite(ctx context.Context, c client.Client, r client.Reader, targetMTU int) (*ReconcileReport, error) {
	if r == nil {
		r = c
	}
	report := &ReconcileReport{EffectiveMTU: targetMTU}
	cniReqs := []corev1.NodeSelectorRequirement{
		Requirement([]string{"aws-vpc"}),
		cloudWorkers,
	}

	linuxDS := &appsv1.DaemonSet{}
	err := r.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: calicoAgent}, linuxDS)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading kube-system/calico-node: %w", err)
	}
	if err == nil {
		patched := linuxDS.DeepCopy()
		changed, effMTU, changes, alignErr := AlignLinuxCalicoMTU(patched, targetMTU)
		if alignErr != nil {
			return nil, alignErr
		}
		if effMTU >= 1280 {
			report.EffectiveMTU = effMTU
		}
		exclChanged := false
		if !excludes(&patched.Spec.Template.Spec, cniReqs) {
			exclude(&patched.Spec.Template.Spec, cniReqs)
			exclChanged = true
			changes = append(changes, "calico-node nodeAffinity exclude cloud-worker and aws-vpc")
		}
		if (changed || exclChanged) && installationOwner(linuxDS) == "" && c != nil {
			if err := c.Patch(ctx, patched, client.MergeFromWithOptions(linuxDS, client.MergeFromWithOptimisticLock{})); err != nil {
				return nil, fmt.Errorf("patching kube-system/calico-node: %w", err)
			}
			if changed {
				report.LinuxDaemonSetPatched = true
			}
			if exclChanged {
				report.ExclusionsPatched = true
			}
			report.Changes = append(report.Changes, changes...)
		} else if (changed || exclChanged) && c == nil {
			if changed {
				report.LinuxDaemonSetPatched = true
			}
			if exclChanged {
				report.ExclusionsPatched = true
			}
			report.Changes = append(report.Changes, changes...)
		}
		if gitopsChanges, gitopsErr := EnsureGitOpsDaemonSetExclusion(ctx, c, r, patched, cniReqs); gitopsErr != nil {
			return nil, gitopsErr
		} else if len(gitopsChanges) > 0 {
			report.GitOpsPatched = true
			report.Changes = append(report.Changes, gitopsChanges...)
		}
	}

	if ipamChanged, ipamChanges, ipamErr := reconcileIPAMConfigDefault(ctx, c, r); ipamErr != nil {
		return nil, ipamErr
	} else if ipamChanged {
		report.IPAMConfigPatched = true
		report.Changes = append(report.Changes, ipamChanges...)
	}

	winCM := &corev1.ConfigMap{}
	err = r.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "calico-windows-config-actual"}, winCM)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading kube-system/calico-windows-config-actual: %w", err)
	}
	if err == nil {
		patchedCM := winCM.DeepCopy()
		changed, changes := AlignWindowsBGPConfigMap(patchedCM, report.EffectiveMTU)
		if changed && c != nil {
			if err := c.Patch(ctx, patchedCM, client.MergeFromWithOptions(winCM, client.MergeFromWithOptimisticLock{})); err != nil {
				return nil, fmt.Errorf("patching kube-system/calico-windows-config-actual: %w", err)
			}
			report.WindowsConfigPatched = true
			report.Changes = append(report.Changes, changes...)
		} else if changed && c == nil {
			report.WindowsConfigPatched = true
			report.Changes = append(report.Changes, changes...)
		}
	}

	winDS := &appsv1.DaemonSet{}
	err = r.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: windowsCalicoAgent}, winDS)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading kube-system/calico-node-windows: %w", err)
	}
	if err == nil {
		patchedWinDS := winDS.DeepCopy()
		var changed bool
		var changes []string
		if report.EffectiveMTU >= 1280 {
			changed, changes = AlignWindowsCalicoDaemonSetMTU(patchedWinDS, report.EffectiveMTU)
		}
		exclChanged := false
		if !excludes(&patchedWinDS.Spec.Template.Spec, cniReqs) {
			exclude(&patchedWinDS.Spec.Template.Spec, cniReqs)
			exclChanged = true
			changes = append(changes, "calico-node-windows nodeAffinity exclude cloud-worker and aws-vpc")
		}
		if (changed || exclChanged) && installationOwner(winDS) == "" && c != nil {
			if err := c.Patch(ctx, patchedWinDS, client.MergeFromWithOptions(winDS, client.MergeFromWithOptimisticLock{})); err != nil {
				return nil, fmt.Errorf("patching kube-system/calico-node-windows: %w", err)
			}
			if changed {
				report.WindowsDaemonPatched = true
			}
			if exclChanged {
				report.ExclusionsPatched = true
			}
			report.Changes = append(report.Changes, changes...)
		} else if (changed || exclChanged) && c == nil {
			if changed {
				report.WindowsDaemonPatched = true
			}
			if exclChanged {
				report.ExclusionsPatched = true
			}
			report.Changes = append(report.Changes, changes...)
		}
	}

	if exclChanges, exclErr := reconcileWildcardDaemonSets(ctx, c, r); exclErr != nil {
		return nil, exclErr
	} else if len(exclChanges) > 0 {
		for _, ch := range exclChanges {
			if strings.HasPrefix(ch, "Flux ") || strings.HasPrefix(ch, "ArgoCD ") {
				report.GitOpsPatched = true
			} else {
				report.ExclusionsPatched = true
			}
		}
		report.Changes = append(report.Changes, exclChanges...)
	}

	if wlChanges, wlErr := reconcileSingleNodeHostPortWorkloads(ctx, c, r); wlErr != nil {
		return nil, wlErr
	} else if len(wlChanges) > 0 {
		for _, ch := range wlChanges {
			if strings.HasPrefix(ch, "Flux ") || strings.HasPrefix(ch, "ArgoCD ") {
				report.GitOpsPatched = true
			} else {
				report.WorkloadsPatched = true
			}
		}
		report.Changes = append(report.Changes, wlChanges...)
	}

	return report, nil
}

func reconcileIPAMConfigDefault(ctx context.Context, c client.Client, r client.Reader) (bool, []string, error) {
	ipam := &unstructured.Unstructured{}
	ipam.SetGroupVersionKind(schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "IPAMConfig"})
	err := r.Get(ctx, client.ObjectKey{Name: "default"}, ipam)
	if meta.IsNoMatchError(err) {
		return false, nil, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("reading IPAMConfig/default: %w", err)
	}
	strictVal, strictFound, _ := unstructured.NestedBool(ipam.Object, "spec", "strictAffinity")
	autoVal, autoFound, _ := unstructured.NestedBool(ipam.Object, "spec", "autoAllocateBlocks")
	if strictFound && strictVal && autoFound && autoVal {
		return false, nil, nil
	}
	patched := ipam.DeepCopy()
	var changes []string
	if !strictFound || !strictVal {
		_ = unstructured.SetNestedField(patched.Object, true, "spec", "strictAffinity")
		changes = append(changes, fmt.Sprintf("IPAMConfig/default spec.strictAffinity %v -> true", strictVal))
	}
	if !autoFound || !autoVal {
		_ = unstructured.SetNestedField(patched.Object, true, "spec", "autoAllocateBlocks")
		changes = append(changes, fmt.Sprintf("IPAMConfig/default spec.autoAllocateBlocks %v -> true", autoVal))
	}
	if c != nil {
		if err := c.Patch(ctx, patched, client.MergeFromWithOptions(ipam, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, nil, fmt.Errorf("patching IPAMConfig/default: %w", err)
		}
	}
	return true, changes, nil
}

func reconcileWildcardDaemonSets(ctx context.Context, c client.Client, r client.Reader) ([]string, error) {
	var allDS appsv1.DaemonSetList
	if err := r.List(ctx, &allDS); err != nil {
		return nil, nil
	}
	reqs := []corev1.NodeSelectorRequirement{cloudWorkers}
	var changes []string
	for i := range allDS.Items {
		ds := &allDS.Items[i]
		if ds.Namespace == "kube-system" || ds.Labels[platform.ComponentLabel] != "" {
			continue
		}
		toleratesAll := false
		for _, t := range ds.Spec.Template.Spec.Tolerations {
			if t.Operator == corev1.TolerationOpExists && strings.TrimSpace(t.Key) == "" &&
				(t.Effect == "" || t.Effect == corev1.TaintEffectNoSchedule) {
				toleratesAll = true
				break
			}
		}
		if !toleratesAll {
			continue
		}
		if !excludes(&ds.Spec.Template.Spec, reqs) {
			patched := ds.DeepCopy()
			exclude(&patched.Spec.Template.Spec, reqs)
			if c != nil {
				if err := c.Patch(ctx, patched, client.MergeFromWithOptions(ds, client.MergeFromWithOptimisticLock{})); err != nil {
					return nil, fmt.Errorf("excluding cloud-worker from DaemonSet %s/%s: %w", ds.Namespace, ds.Name, err)
				}
			}
			changes = append(changes, fmt.Sprintf("DaemonSet %s/%s nodeAffinity exclude cloud-worker", ds.Namespace, ds.Name))
		}
		if gitopsChanges, err := EnsureGitOpsDaemonSetExclusion(ctx, c, r, ds, reqs); err != nil {
			return nil, err
		} else if len(gitopsChanges) > 0 {
			changes = append(changes, gitopsChanges...)
		}
	}
	return changes, nil
}

func reconcileSingleNodeHostPortWorkloads(ctx context.Context, c client.Client, r client.Reader) ([]string, error) {
	var changes []string

	// 1. Reconcile NginxProxy CRs (gateway.nginx.org/v1alpha2) if served.
	npList := &unstructured.UnstructuredList{}
	npList.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.nginx.org", Version: "v1alpha2", Kind: "NginxProxyList"})
	if err := r.List(ctx, npList); err == nil {
		for i := range npList.Items {
			np := &npList.Items[i]
			hostPorts, found, _ := unstructured.NestedSlice(np.Object, "spec", "kubernetes", "deployment", "container", "hostPorts")
			if !found || len(hostPorts) == 0 {
				continue
			}
			patches, _, _ := unstructured.NestedSlice(np.Object, "spec", "kubernetes", "deployment", "patches")
			hasRecreate := false
			for _, rawPatch := range patches {
				b, _ := json.Marshal(rawPatch)
				if strings.Contains(string(b), "Recreate") || strings.Contains(string(b), `"maxSurge":0`) {
					hasRecreate = true
					break
				}
			}
			if !hasRecreate {
				patched := np.DeepCopy()
				recreatePatch := map[string]interface{}{
					"type": "StrategicMerge",
					"value": map[string]interface{}{
						"spec": map[string]interface{}{
							"strategy": map[string]interface{}{
								"type":          "Recreate",
								"rollingUpdate": nil,
							},
						},
					},
				}
				patches = append(patches, recreatePatch)
				if err := unstructured.SetNestedSlice(patched.Object, patches, "spec", "kubernetes", "deployment", "patches"); err != nil {
					return nil, err
				}
				if c != nil {
					if err := c.Patch(ctx, patched, client.MergeFromWithOptions(np, client.MergeFromWithOptimisticLock{})); err != nil {
						return nil, fmt.Errorf("patching NginxProxy %s/%s: %w", np.GetNamespace(), np.GetName(), err)
					}
				}
				changes = append(changes, fmt.Sprintf("NginxProxy %s/%s added StrategicMerge deployment patch strategy.type=Recreate", np.GetNamespace(), np.GetName()))
			}
			npPatchYAML := fmt.Sprintf("apiVersion: gateway.nginx.org/v1alpha2\nkind: NginxProxy\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n  kubernetes:\n    deployment:\n      patches:\n        - type: StrategicMerge\n          value:\n            spec:\n              strategy:\n                type: Recreate\n                rollingUpdate: null\n", np.GetName(), np.GetNamespace())
			if gitopsChanges, err := EnsureGitOpsWorkloadRecreate(ctx, c, r, np, "gateway.nginx.org", "v1alpha2", "NginxProxy", "/spec/kubernetes/deployment/patches", npPatchYAML); err != nil {
				return nil, err
			} else if len(gitopsChanges) > 0 {
				changes = append(changes, gitopsChanges...)
			}
		}
	}

	// 2. Reconcile single-node / cloud-worker hostPort Deployments.
	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps); err != nil {
		return changes, nil
	}
	for i := range deps.Items {
		dep := &deps.Items[i]
		if len(dep.OwnerReferences) > 0 {
			// Owned by a higher-level controller (e.g. NginxGateway); only patch strategy if still RollingUpdate.
		}
		singleNode := dep.Spec.Template.Spec.NodeSelector[platform.RoleLabel] == platform.RoleValue ||
			dep.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != ""
		if !singleNode {
			for _, tol := range dep.Spec.Template.Spec.Tolerations {
				if tol.Key == "cloud-provisioning.appmana.com/cloud-worker" || tol.Key == platform.RoleLabel {
					singleNode = true
					break
				}
			}
		}
		if !singleNode {
			continue
		}
		hasHostPort := false
		for _, container := range dep.Spec.Template.Spec.Containers {
			for _, p := range container.Ports {
				if p.HostPort > 0 {
					hasHostPort = true
					break
				}
			}
		}
		if !hasHostPort {
			continue
		}
		alreadyRecreate := dep.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType
		if !alreadyRecreate && dep.Spec.Strategy.RollingUpdate != nil && dep.Spec.Strategy.RollingUpdate.MaxSurge != nil {
			ms := dep.Spec.Strategy.RollingUpdate.MaxSurge.String()
			if ms == "0" || ms == "0%" {
				alreadyRecreate = true
			}
		}
		if !alreadyRecreate {
			patched := dep.DeepCopy()
			patched.Spec.Strategy = appsv1.DeploymentStrategy{
				Type:          appsv1.RecreateDeploymentStrategyType,
				RollingUpdate: nil,
			}
			if c != nil {
				if err := c.Patch(ctx, patched, client.MergeFromWithOptions(dep, client.MergeFromWithOptimisticLock{})); err != nil {
					return nil, fmt.Errorf("patching Deployment %s/%s strategy: %w", dep.Namespace, dep.Name, err)
				}
			}
			changes = append(changes, fmt.Sprintf("Deployment %s/%s strategy.type RollingUpdate -> Recreate", dep.Namespace, dep.Name))
		}
		depPatchYAML := fmt.Sprintf("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n  strategy:\n    type: Recreate\n    rollingUpdate: null\n", dep.Name, dep.Namespace)
		if gitopsChanges, err := EnsureGitOpsWorkloadRecreate(ctx, c, r, dep, "apps", "v1", "Deployment", "/spec/strategy", depPatchYAML); err != nil {
			return nil, err
		} else if len(gitopsChanges) > 0 {
			changes = append(changes, gitopsChanges...)
		}
	}
	return changes, nil
}

// ensureCalicoAlignment reconciles MTU and Windows BGP ConfigMap alignment on a
// Calico site before TunnelAgent clones the site DaemonSet.
func (e *Excluder) ensureCalicoAlignment(ctx context.Context, ds *appsv1.DaemonSet) error {
	if ds == nil || installationOwner(ds) != "" {
		return nil
	}
	switch ds.Name {
	case calicoAgent:
		patched := ds.DeepCopy()
		changed, effMTU, changes, err := AlignLinuxCalicoMTU(patched, 0)
		if err != nil {
			return err
		}
		if changed {
			if err := e.Client.Patch(ctx, patched, client.MergeFromWithOptions(ds, client.MergeFromWithOptimisticLock{})); err != nil {
				return fmt.Errorf("aligning %s MTU: %w", client.ObjectKeyFromObject(ds), err)
			}
			*ds = *patched
			logf.FromContext(ctx).Info("aligned site Calico MTU", "daemonSet", client.ObjectKeyFromObject(ds), "effectiveMTU", effMTU, "changes", changes)
		}
		if ipamChanged, ipamChanges, ipamErr := reconcileIPAMConfigDefault(ctx, e.Client, e.Reader); ipamErr != nil {
			return ipamErr
		} else if ipamChanged {
			logf.FromContext(ctx).Info("reconciled IPAMConfig/default", "changes", ipamChanges)
		}
		if effMTU >= 1280 {
			winCM := &corev1.ConfigMap{}
			if err := e.Reader.Get(ctx, client.ObjectKey{Namespace: ds.Namespace, Name: "calico-windows-config-actual"}, winCM); err == nil {
				patchedCM := winCM.DeepCopy()
				if cmChanged, cmChanges := AlignWindowsBGPConfigMap(patchedCM, effMTU); cmChanged {
					if err := e.Client.Patch(ctx, patchedCM, client.MergeFromWithOptions(winCM, client.MergeFromWithOptimisticLock{})); err != nil {
						return fmt.Errorf("aligning %s/calico-windows-config-actual: %w", ds.Namespace, err)
					}
					logf.FromContext(ctx).Info("aligned Windows BGP ConfigMap", "configMap", client.ObjectKeyFromObject(winCM), "changes", cmChanges)
				}
			}
		}
	case windowsCalicoAgent:
		var effMTU int
		linuxDS := &appsv1.DaemonSet{}
		if err := e.Reader.Get(ctx, client.ObjectKey{Namespace: ds.Namespace, Name: calicoAgent}, linuxDS); err == nil {
			_, effMTU, _, _ = AlignLinuxCalicoMTU(linuxDS.DeepCopy(), 0)
		}
		winCM := &corev1.ConfigMap{}
		if err := e.Reader.Get(ctx, client.ObjectKey{Namespace: ds.Namespace, Name: "calico-windows-config-actual"}, winCM); err == nil {
			patchedCM := winCM.DeepCopy()
			if cmChanged, cmChanges := AlignWindowsBGPConfigMap(patchedCM, effMTU); cmChanged {
				if err := e.Client.Patch(ctx, patchedCM, client.MergeFromWithOptions(winCM, client.MergeFromWithOptimisticLock{})); err != nil {
					return fmt.Errorf("aligning %s/calico-windows-config-actual: %w", ds.Namespace, err)
				}
				logf.FromContext(ctx).Info("aligned Windows BGP ConfigMap", "configMap", client.ObjectKeyFromObject(winCM), "changes", cmChanges)
			}
		}
		if effMTU >= 1280 {
			patched := ds.DeepCopy()
			if changed, changes := AlignWindowsCalicoDaemonSetMTU(patched, effMTU); changed {
				if err := e.Client.Patch(ctx, patched, client.MergeFromWithOptions(ds, client.MergeFromWithOptimisticLock{})); err != nil {
					return fmt.Errorf("aligning %s MTU: %w", client.ObjectKeyFromObject(ds), err)
				}
				*ds = *patched
				logf.FromContext(ctx).Info("aligned Windows Calico DaemonSet MTU", "daemonSet", client.ObjectKeyFromObject(ds), "changes", changes)
			}
		}
	}
	return nil
}
