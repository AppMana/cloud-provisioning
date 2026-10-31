package site

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

var (
	fluxHelmReleaseGVK   = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}
	fluxKustomizationGVK = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}
	argoApplicationGVK   = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}
)

// GitOpsOwner identifies the Flux or ArgoCD controller resource that manages
// a cluster object so automated fixes can be persisted on the upstream CR
// (preventing GitOps reconciliation or self-heal from reverting them).
type GitOpsOwner struct {
	// System is "flux-helmrelease", "flux-kustomization", or "argocd-application".
	System    string
	Namespace string
	Name      string
}

func (o GitOpsOwner) String() string {
	if o.Namespace != "" {
		return fmt.Sprintf("%s %s/%s", o.System, o.Namespace, o.Name)
	}
	return fmt.Sprintf("%s %s", o.System, o.Name)
}

// DetectGitOpsOwners returns all GitOps owner references (Flux HelmRelease,
// Flux Kustomization, ArgoCD Application) stamped on obj's metadata.
func DetectGitOpsOwners(obj client.Object) []GitOpsOwner {
	if obj == nil {
		return nil
	}
	labels := obj.GetLabels()
	annotations := obj.GetAnnotations()
	var owners []GitOpsOwner

	if hrName := strings.TrimSpace(labels["helm.toolkit.fluxcd.io/name"]); hrName != "" {
		hrNS := strings.TrimSpace(labels["helm.toolkit.fluxcd.io/namespace"])
		if hrNS == "" {
			hrNS = obj.GetNamespace()
		}
		owners = append(owners, GitOpsOwner{
			System:    "flux-helmrelease",
			Namespace: hrNS,
			Name:      hrName,
		})
	}

	if ksName := strings.TrimSpace(labels["kustomize.toolkit.fluxcd.io/name"]); ksName != "" {
		ksNS := strings.TrimSpace(labels["kustomize.toolkit.fluxcd.io/namespace"])
		if ksNS == "" {
			ksNS = "flux-system"
		}
		owners = append(owners, GitOpsOwner{
			System:    "flux-kustomization",
			Namespace: ksNS,
			Name:      ksName,
		})
	}

	argoApp := strings.TrimSpace(labels["argocd.argoproj.io/instance"])
	if argoApp == "" {
		if tracking := strings.TrimSpace(annotations["argocd.argoproj.io/tracking-id"]); tracking != "" {
			if idx := strings.IndexByte(tracking, ':'); idx > 0 {
				argoApp = tracking[:idx]
			}
		}
	}
	if argoApp == "" {
		if inst := strings.TrimSpace(labels["app.kubernetes.io/instance"]); inst != "" {
			for _, mf := range obj.GetManagedFields() {
				if strings.Contains(strings.ToLower(mf.Manager), "argocd") {
					argoApp = inst
					break
				}
			}
		}
	}
	if argoApp != "" {
		owners = append(owners, GitOpsOwner{
			System:    "argocd-application",
			Namespace: "argocd",
			Name:      argoApp,
		})
	}
	return owners
}

// EnsureGitOpsDaemonSetExclusion persists a DaemonSet's nodeAffinity exclusion
// onto its upstream Flux HelmRelease (via spec.postRenderers), Flux Kustomization
// (via spec.patches), or ArgoCD Application (via spec.ignoreDifferences) so
// GitOps reconciliation does not revert the exclusion.
func EnsureGitOpsDaemonSetExclusion(ctx context.Context, c client.Client, r client.Reader, ds *appsv1.DaemonSet, reqs []corev1.NodeSelectorRequirement) ([]string, error) {
	if r == nil || ds == nil || len(reqs) == 0 {
		return nil, nil
	}
	owners := DetectGitOpsOwners(ds)
	if len(owners) == 0 {
		return nil, nil
	}
	specCopy := ds.Spec.Template.Spec.DeepCopy()
	exclude(specCopy, reqs)
	affinityMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(specCopy.Affinity)
	if err != nil {
		return nil, err
	}
	patchDoc := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata": map[string]interface{}{
			"name":      ds.Name,
			"namespace": ds.Namespace,
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"affinity": affinityMap,
				},
			},
		},
	}
	patchBytes, err := yaml.Marshal(patchDoc)
	if err != nil {
		return nil, err
	}
	target := map[string]interface{}{
		"group":     "apps",
		"version":   "v1",
		"kind":      "DaemonSet",
		"name":      ds.Name,
		"namespace": ds.Namespace,
	}

	var changes []string
	for _, owner := range owners {
		switch owner.System {
		case "flux-helmrelease":
			changed, err := ensureFluxHelmReleasePostRenderer(ctx, c, r, owner, target, string(patchBytes), reqs)
			if err != nil {
				return changes, err
			}
			if changed {
				changes = append(changes, fmt.Sprintf("Flux HelmRelease %s/%s added postRenderer excluding cloud nodes on DaemonSet %s/%s", owner.Namespace, owner.Name, ds.Namespace, ds.Name))
			}
		case "flux-kustomization":
			changed, err := ensureFluxKustomizationPatch(ctx, c, r, owner, target, string(patchBytes), platform.RoleValue)
			if err != nil {
				return changes, err
			}
			if changed {
				changes = append(changes, fmt.Sprintf("Flux Kustomization %s/%s added spec.patches excluding cloud nodes on DaemonSet %s/%s", owner.Namespace, owner.Name, ds.Namespace, ds.Name))
			}
		case "argocd-application":
			changed, err := ensureArgoApplicationIgnoreDiff(ctx, c, r, owner, "apps", "DaemonSet", ds.Namespace, ds.Name, "/spec/template/spec/affinity")
			if err != nil {
				return changes, err
			}
			if changed {
				changes = append(changes, fmt.Sprintf("ArgoCD Application %s/%s added ignoreDifferences for DaemonSet %s/%s /spec/template/spec/affinity", owner.Namespace, owner.Name, ds.Namespace, ds.Name))
			}
		}
	}
	return changes, nil
}

// EnsureGitOpsWorkloadRecreate persists a single-node hostPort NginxProxy or
// Deployment Recreate rollout strategy onto its upstream Flux Kustomization,
// Flux HelmRelease, or ArgoCD Application.
func EnsureGitOpsWorkloadRecreate(ctx context.Context, c client.Client, r client.Reader, obj client.Object, group, version, kind, jsonPointer, patchYAML string) ([]string, error) {
	if r == nil || obj == nil {
		return nil, nil
	}
	owners := DetectGitOpsOwners(obj)
	if len(owners) == 0 {
		return nil, nil
	}
	target := map[string]interface{}{
		"group":     group,
		"version":   version,
		"kind":      kind,
		"name":      obj.GetName(),
		"namespace": obj.GetNamespace(),
	}
	var changes []string
	for _, owner := range owners {
		switch owner.System {
		case "flux-helmrelease":
			changed, err := ensureFluxHelmReleasePostRenderer(ctx, c, r, owner, target, patchYAML, nil)
			if err != nil {
				return changes, err
			}
			if changed {
				changes = append(changes, fmt.Sprintf("Flux HelmRelease %s/%s added postRenderer strategy.type=Recreate for %s %s/%s", owner.Namespace, owner.Name, kind, obj.GetNamespace(), obj.GetName()))
			}
		case "flux-kustomization":
			changed, err := ensureFluxKustomizationPatch(ctx, c, r, owner, target, patchYAML, "Recreate")
			if err != nil {
				return changes, err
			}
			if changed {
				changes = append(changes, fmt.Sprintf("Flux Kustomization %s/%s added spec.patches strategy.type=Recreate for %s %s/%s", owner.Namespace, owner.Name, kind, obj.GetNamespace(), obj.GetName()))
			}
		case "argocd-application":
			changed, err := ensureArgoApplicationIgnoreDiff(ctx, c, r, owner, group, kind, obj.GetNamespace(), obj.GetName(), jsonPointer)
			if err != nil {
				return changes, err
			}
			if changed {
				changes = append(changes, fmt.Sprintf("ArgoCD Application %s/%s added ignoreDifferences for %s %s/%s %s", owner.Namespace, owner.Name, kind, obj.GetNamespace(), obj.GetName(), jsonPointer))
			}
		}
	}
	return changes, nil
}

// CheckGitOpsDaemonSetExclusion verifies whether a GitOps-managed DaemonSet's
// upstream Flux HelmRelease, Flux Kustomization, or ArgoCD Application persists
// its nodeAffinity exclusion so future GitOps syncs or Helm upgrades will not
// revert it.
func CheckGitOpsDaemonSetExclusion(ctx context.Context, r client.Reader, ds *appsv1.DaemonSet, reqKey, reqVal string) []string {
	if r == nil || ds == nil {
		return nil
	}
	var warnings []string
	for _, owner := range DetectGitOpsOwners(ds) {
		switch owner.System {
		case "flux-helmrelease":
			hr := &unstructured.Unstructured{}
			hr.SetGroupVersionKind(fluxHelmReleaseGVK)
			if err := r.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, hr); err != nil {
				continue
			}
			if !fluxHelmReleaseHasPatch(hr, ds.Name, reqVal) {
				warnings = append(warnings, fmt.Sprintf(
					"DaemonSet %s/%s is managed by Flux HelmRelease %s/%s which does not yet persist the %s=%s exclusion in spec.postRenderers or spec.values.affinity; a future Helm upgrade or drift check will revert it (run 'platform-tuple fix' and commit to GitOps)",
					ds.Namespace, ds.Name, owner.Namespace, owner.Name, reqKey, reqVal,
				))
			}
		case "flux-kustomization":
			ks := &unstructured.Unstructured{}
			ks.SetGroupVersionKind(fluxKustomizationGVK)
			if err := r.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, ks); err != nil {
				continue
			}
			if fluxKustomizationOwnsField(ds, "f:affinity") && !fluxKustomizationHasPatch(ks, "DaemonSet", ds.Name, reqVal) {
				warnings = append(warnings, fmt.Sprintf(
					"DaemonSet %s/%s is managed by Flux Kustomization %s/%s which owns spec.template.spec.affinity without a matching spec.patches entry for %s=%s; Flux reconciliation will revert it (run 'platform-tuple fix' and commit to GitOps)",
					ds.Namespace, ds.Name, owner.Namespace, owner.Name, reqKey, reqVal,
				))
			}
		case "argocd-application":
			app := &unstructured.Unstructured{}
			app.SetGroupVersionKind(argoApplicationGVK)
			if err := r.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, app); err != nil {
				continue
			}
			if !argoApplicationHasIgnoreDiff(app, "DaemonSet", ds.Name, "/spec/template/spec/affinity") {
				warnings = append(warnings, fmt.Sprintf(
					"DaemonSet %s/%s is managed by ArgoCD Application %s/%s without ignoreDifferences for /spec/template/spec/affinity; ArgoCD self-heal may revert the exclusion (run 'platform-tuple fix' and commit to GitOps)",
					ds.Namespace, ds.Name, owner.Namespace, owner.Name,
				))
			}
		}
	}
	return warnings
}

// CheckGitOpsWorkloadRecreate verifies whether a GitOps-managed NginxProxy or
// single-node hostPort Deployment persists its Recreate rollout strategy in its
// upstream Flux Kustomization, Flux HelmRelease, or ArgoCD Application.
func CheckGitOpsWorkloadRecreate(ctx context.Context, r client.Reader, obj client.Object, kind, jsonPointer string) []string {
	if r == nil || obj == nil {
		return nil
	}
	var warnings []string
	for _, owner := range DetectGitOpsOwners(obj) {
		switch owner.System {
		case "flux-helmrelease":
			hr := &unstructured.Unstructured{}
			hr.SetGroupVersionKind(fluxHelmReleaseGVK)
			if err := r.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, hr); err != nil {
				continue
			}
			if !fluxHelmReleaseHasPatch(hr, obj.GetName(), "Recreate") {
				warnings = append(warnings, fmt.Sprintf(
					"%s %s/%s is managed by Flux HelmRelease %s/%s which does not yet persist strategy.type=Recreate in spec.postRenderers or spec.values; a future Helm upgrade will revert it (run 'platform-tuple fix' and commit to GitOps)",
					kind, obj.GetNamespace(), obj.GetName(), owner.Namespace, owner.Name,
				))
			}
		case "flux-kustomization":
			ks := &unstructured.Unstructured{}
			ks.SetGroupVersionKind(fluxKustomizationGVK)
			if err := r.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, ks); err != nil {
				continue
			}
			if !fluxKustomizationHasPatch(ks, kind, obj.GetName(), "Recreate") && !fluxObjectHasGitCommittedField(obj, kind) {
				warnings = append(warnings, fmt.Sprintf(
					"%s %s/%s is managed by Flux Kustomization %s/%s which does not yet include strategy.type=Recreate in spec.patches (or in the applied Git revision); a future GitOps sync may revert it (run 'platform-tuple fix' and commit to GitOps)",
					kind, obj.GetNamespace(), obj.GetName(), owner.Namespace, owner.Name,
				))
			}
		case "argocd-application":
			app := &unstructured.Unstructured{}
			app.SetGroupVersionKind(argoApplicationGVK)
			if err := r.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, app); err != nil {
				continue
			}
			if !argoApplicationHasIgnoreDiff(app, kind, obj.GetName(), jsonPointer) {
				warnings = append(warnings, fmt.Sprintf(
					"%s %s/%s is managed by ArgoCD Application %s/%s without ignoreDifferences for %s; ArgoCD self-heal may revert strategy.type=Recreate (run 'platform-tuple fix' and commit to GitOps)",
					kind, obj.GetNamespace(), obj.GetName(), owner.Namespace, owner.Name, jsonPointer,
				))
			}
		}
	}
	return warnings
}

func fluxObjectHasGitCommittedField(obj client.Object, kind string) bool {
	for _, mf := range obj.GetManagedFields() {
		if mf.Manager != "kustomize-controller" || mf.FieldsV1 == nil {
			continue
		}
		raw := string(mf.FieldsV1.Raw)
		switch kind {
		case "NginxProxy":
			if strings.Contains(raw, `"f:patches"`) {
				return true
			}
		case "Deployment":
			if strings.Contains(raw, `"f:strategy"`) {
				return true
			}
		}
	}
	return false
}

func fluxKustomizationOwnsField(obj client.Object, fieldKey string) bool {
	for _, mf := range obj.GetManagedFields() {
		if mf.Manager != "kustomize-controller" || mf.FieldsV1 == nil {
			continue
		}
		if strings.Contains(string(mf.FieldsV1.Raw), fieldKey) {
			return true
		}
	}
	return false
}

func fluxHelmReleaseHasPatch(hr *unstructured.Unstructured, targetName, needle string) bool {
	raw, err := json.Marshal(hr.Object["spec"])
	if err != nil {
		return false
	}
	s := string(raw)
	if strings.Contains(s, needle) && (targetName == "" || strings.Contains(s, targetName)) {
		return true
	}
	return false
}

func fluxKustomizationHasPatch(ks *unstructured.Unstructured, targetKind, targetName, needle string) bool {
	patches, found, _ := unstructured.NestedSlice(ks.Object, "spec", "patches")
	if !found {
		return false
	}
	for _, item := range patches {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		patchStr, _, _ := unstructured.NestedString(m, "patch")
		tKind, _, _ := unstructured.NestedString(m, "target", "kind")
		tName, _, _ := unstructured.NestedString(m, "target", "name")
		if (tKind == "" || tKind == targetKind) && (tName == "" || tName == targetName) && strings.Contains(patchStr, needle) {
			return true
		}
	}
	return false
}

func argoApplicationHasIgnoreDiff(app *unstructured.Unstructured, kind, name, jsonPointer string) bool {
	diffs, found, _ := unstructured.NestedSlice(app.Object, "spec", "ignoreDifferences")
	if !found {
		return false
	}
	for _, item := range diffs {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		dKind, _, _ := unstructured.NestedString(m, "kind")
		dName, _, _ := unstructured.NestedString(m, "name")
		if dKind != kind || (dName != "" && dName != name) {
			continue
		}
		ptrs, _, _ := unstructured.NestedStringSlice(m, "jsonPointers")
		for _, p := range ptrs {
			if p == jsonPointer || strings.HasPrefix(jsonPointer, p+"/") {
				return true
			}
		}
	}
	return false
}

func ensureFluxHelmReleasePostRenderer(ctx context.Context, c client.Client, r client.Reader, owner GitOpsOwner, target map[string]interface{}, patchYAML string, reqs []corev1.NodeSelectorRequirement) (bool, error) {
	hr := &unstructured.Unstructured{}
	hr.SetGroupVersionKind(fluxHelmReleaseGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, hr); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	targetName, _ := target["name"].(string)
	needle := "Recreate"
	if len(reqs) > 0 && len(reqs[0].Values) > 0 {
		needle = reqs[0].Values[0]
	}
	if fluxHelmReleaseHasPatch(hr, targetName, needle) {
		return false, nil
	}
	patched := hr.DeepCopy()
	postRenderers, _, _ := unstructured.NestedSlice(patched.Object, "spec", "postRenderers")
	newEntry := map[string]interface{}{
		"kustomize": map[string]interface{}{
			"patches": []interface{}{
				map[string]interface{}{
					"target": target,
					"patch":  patchYAML,
				},
			},
		},
	}
	postRenderers = append(postRenderers, newEntry)
	if err := unstructured.SetNestedSlice(patched.Object, postRenderers, "spec", "postRenderers"); err != nil {
		return false, err
	}
	if c != nil {
		if err := c.Patch(ctx, patched, client.MergeFromWithOptions(hr, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, fmt.Errorf("patching Flux HelmRelease %s/%s: %w", owner.Namespace, owner.Name, err)
		}
	}
	return true, nil
}

func ensureFluxKustomizationPatch(ctx context.Context, c client.Client, r client.Reader, owner GitOpsOwner, target map[string]interface{}, patchYAML, needle string) (bool, error) {
	ks := &unstructured.Unstructured{}
	ks.SetGroupVersionKind(fluxKustomizationGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, ks); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	targetKind, _ := target["kind"].(string)
	targetName, _ := target["name"].(string)
	if fluxKustomizationHasPatch(ks, targetKind, targetName, needle) {
		return false, nil
	}
	patched := ks.DeepCopy()
	patches, _, _ := unstructured.NestedSlice(patched.Object, "spec", "patches")
	patches = append(patches, map[string]interface{}{
		"target": target,
		"patch":  patchYAML,
	})
	if err := unstructured.SetNestedSlice(patched.Object, patches, "spec", "patches"); err != nil {
		return false, err
	}
	if c != nil {
		if err := c.Patch(ctx, patched, client.MergeFromWithOptions(ks, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, fmt.Errorf("patching Flux Kustomization %s/%s: %w", owner.Namespace, owner.Name, err)
		}
	}
	return true, nil
}

func ensureArgoApplicationIgnoreDiff(ctx context.Context, c client.Client, r client.Reader, owner GitOpsOwner, group, kind, namespace, name, jsonPointer string) (bool, error) {
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argoApplicationGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, app); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	if argoApplicationHasIgnoreDiff(app, kind, name, jsonPointer) {
		return false, nil
	}
	patched := app.DeepCopy()
	diffs, _, _ := unstructured.NestedSlice(patched.Object, "spec", "ignoreDifferences")
	diffs = append(diffs, map[string]interface{}{
		"group":        group,
		"kind":         kind,
		"name":         name,
		"namespace":    namespace,
		"jsonPointers": []interface{}{jsonPointer},
	})
	if err := unstructured.SetNestedSlice(patched.Object, diffs, "spec", "ignoreDifferences"); err != nil {
		return false, err
	}
	if c != nil {
		if err := c.Patch(ctx, patched, client.MergeFromWithOptions(app, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, fmt.Errorf("patching ArgoCD Application %s/%s: %w", owner.Namespace, owner.Name, err)
		}
	}
	return true, nil
}

// ReconcileGitOpsManifestFiles inspects YAML manifest files or directories on
// disk (e.g. a Flux or ArgoCD GitOps repository checkout) and automatically
// remediates:
//   - NginxProxy manifests with hostPorts missing a StrategicMerge Recreate patch
//   - Single-node / cloud-worker Deployment manifests with hostPort using RollingUpdate
//   - Flux HelmRelease manifests for spegel (or wildcard-toleration agents) missing cloud-worker nodeAffinity exclusion
//   - DaemonSet manifests with wildcard taint tolerations missing cloud-worker nodeAffinity exclusion
//
// When dryRun is true, it reports the planned file changes without writing to disk.
func ReconcileGitOpsManifestFiles(paths []string, dryRun bool) ([]string, error) {
	files, err := expandYAMLFilePaths(paths)
	if err != nil {
		return nil, err
	}
	var changes []string
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return changes, err
		}
		updated, fileChanges, changed := patchGitOpsYAMLDocumentSet(file, raw)
		if !changed {
			continue
		}
		if !dryRun {
			if err := os.WriteFile(file, updated, 0o644); err != nil {
				return changes, err
			}
		}
		changes = append(changes, fileChanges...)
	}
	return changes, nil
}

func expandYAMLFilePaths(paths []string) ([]string, error) {
	var out []string
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
				out = append(out, p)
				continue
			}
			err = filepath.WalkDir(p, func(cur string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.IsDir() {
					if strings.HasPrefix(d.Name(), ".") {
						return filepath.SkipDir
					}
					return nil
				}
				ext := strings.ToLower(filepath.Ext(d.Name()))
				if ext == ".yaml" || ext == ".yml" {
					out = append(out, cur)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func patchGitOpsYAMLDocumentSet(filePath string, raw []byte) ([]byte, []string, bool) {
	docs := strings.Split(string(raw), "\n---")
	var outDocs []string
	var changes []string
	anyChanged := false

	for _, doc := range docs {
		var obj map[string]interface{}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil || len(obj) == 0 {
			outDocs = append(outDocs, doc)
			continue
		}
		kind, _ := obj["kind"].(string)
		metaMap, _ := obj["metadata"].(map[string]interface{})
		name, _ := metaMap["name"].(string)
		ns, _ := metaMap["namespace"].(string)

		switch kind {
		case "NginxProxy":
			hostPorts, found, _ := unstructured.NestedSlice(obj, "spec", "kubernetes", "deployment", "container", "hostPorts")
			if !found || len(hostPorts) == 0 {
				outDocs = append(outDocs, doc)
				continue
			}
			patches, _, _ := unstructured.NestedSlice(obj, "spec", "kubernetes", "deployment", "patches")
			hasRecreate := false
			for _, p := range patches {
				if pm, ok := p.(map[string]interface{}); ok {
					t, _, _ := unstructured.NestedString(pm, "value", "spec", "strategy", "type")
					if strings.EqualFold(t, "Recreate") {
						hasRecreate = true
						break
					}
				}
			}
			if hasRecreate {
				outDocs = append(outDocs, doc)
				continue
			}
			patches = append(patches, map[string]interface{}{
				"type": "StrategicMerge",
				"value": map[string]interface{}{
					"spec": map[string]interface{}{
						"strategy": map[string]interface{}{
							"type":          "Recreate",
							"rollingUpdate": nil,
						},
					},
				},
			})
			_ = unstructured.SetNestedSlice(obj, patches, "spec", "kubernetes", "deployment", "patches")
			marshaled, err := yaml.Marshal(obj)
			if err != nil {
				outDocs = append(outDocs, doc)
				continue
			}
			outDocs = append(outDocs, "\n"+strings.TrimRight(string(marshaled), "\n"))
			anyChanged = true
			changes = append(changes, fmt.Sprintf("manifest %s: NginxProxy %s/%s added StrategicMerge deployment patch strategy.type=Recreate", filePath, ns, name))

		case "HelmRelease":
			chartName, _, _ := unstructured.NestedString(obj, "spec", "chart", "spec", "chart")
			if chartName != "spegel" && name != "spegel" {
				outDocs = append(outDocs, doc)
				continue
			}
			rawSpec, _ := json.Marshal(obj["spec"])
			if strings.Contains(string(rawSpec), platform.RoleValue) {
				outDocs = append(outDocs, doc)
				continue
			}
			affinity := map[string]interface{}{
				"nodeAffinity": map[string]interface{}{
					"requiredDuringSchedulingIgnoredDuringExecution": map[string]interface{}{
						"nodeSelectorTerms": []interface{}{
							map[string]interface{}{
								"matchExpressions": []interface{}{
									map[string]interface{}{
										"key":      platform.RoleLabel,
										"operator": "NotIn",
										"values":   []interface{}{platform.RoleValue},
									},
								},
							},
						},
					},
				},
			}
			_ = unstructured.SetNestedMap(obj, affinity, "spec", "values", "affinity")
			marshaled, err := yaml.Marshal(obj)
			if err != nil {
				outDocs = append(outDocs, doc)
				continue
			}
			outDocs = append(outDocs, "\n"+strings.TrimRight(string(marshaled), "\n"))
			anyChanged = true
			changes = append(changes, fmt.Sprintf("manifest %s: HelmRelease %s/%s added values.affinity excluding %s=%s", filePath, ns, name, platform.RoleLabel, platform.RoleValue))

		default:
			outDocs = append(outDocs, doc)
		}
	}
	if !anyChanged {
		return raw, nil, false
	}
	joined := strings.Join(outDocs, "\n---")
	joined = strings.TrimLeft(joined, "\n")
	if !strings.HasSuffix(joined, "\n") {
		joined += "\n"
	}
	return []byte(joined), changes, true
}
