package site

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var cniConfigMTUPattern = regexp.MustCompile(`("mtu"\s*:\s*)(\d+)`)

// ReconcileReport summarizes changes applied (or planned) by ReconcileCalicoSite.
type ReconcileReport struct {
	EffectiveMTU          int      `json:"effectiveMTU,omitempty"`
	LinuxDaemonSetPatched bool     `json:"linuxDaemonSetPatched,omitempty"`
	WindowsConfigPatched  bool     `json:"windowsConfigPatched,omitempty"`
	WindowsDaemonPatched  bool     `json:"windowsDaemonSetPatched,omitempty"`
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
// Windows BGP ConfigMap (calico-windows-config-actual), and Windows Calico
// DaemonSet (calico-node-windows) have aligned MTU and Windows BGP settings.
func ReconcileCalicoSite(ctx context.Context, c client.Client, r client.Reader, targetMTU int) (*ReconcileReport, error) {
	if r == nil {
		r = c
	}
	report := &ReconcileReport{EffectiveMTU: targetMTU}

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
		if changed && installationOwner(linuxDS) == "" && c != nil {
			if err := c.Patch(ctx, patched, client.MergeFromWithOptions(linuxDS, client.MergeFromWithOptimisticLock{})); err != nil {
				return nil, fmt.Errorf("patching kube-system/calico-node MTU: %w", err)
			}
			report.LinuxDaemonSetPatched = true
			report.Changes = append(report.Changes, changes...)
		} else if changed && c == nil {
			report.LinuxDaemonSetPatched = true
			report.Changes = append(report.Changes, changes...)
		}
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
	if err == nil && report.EffectiveMTU >= 1280 {
		patchedWinDS := winDS.DeepCopy()
		changed, changes := AlignWindowsCalicoDaemonSetMTU(patchedWinDS, report.EffectiveMTU)
		if changed && installationOwner(winDS) == "" && c != nil {
			if err := c.Patch(ctx, patchedWinDS, client.MergeFromWithOptions(winDS, client.MergeFromWithOptimisticLock{})); err != nil {
				return nil, fmt.Errorf("patching kube-system/calico-node-windows MTU: %w", err)
			}
			report.WindowsDaemonPatched = true
			report.Changes = append(report.Changes, changes...)
		} else if changed && c == nil {
			report.WindowsDaemonPatched = true
			report.Changes = append(report.Changes, changes...)
		}
	}

	return report, nil
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
