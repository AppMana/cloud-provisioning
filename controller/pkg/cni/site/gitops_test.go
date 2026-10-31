package site

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func gitopsTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sc := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{
		fluxHelmReleaseGVK,
		fluxKustomizationGVK,
		argoApplicationGVK,
		{Group: "gateway.nginx.org", Version: "v1alpha2", Kind: "NginxProxy"},
		{Group: "crd.projectcalico.org", Version: "v1", Kind: "IPAMConfig"},
	} {
		sc.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		sc.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return fake.NewClientBuilder().WithScheme(sc).WithObjects(objs...).Build()
}

func TestGitOpsReconcilePersistsToFluxHelmReleaseKustomizationAndArgoCD(t *testing.T) {
	ctx := context.Background()

	// 1. DaemonSet spegel managed by Flux HelmRelease spegel/spegel
	spegelDS := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spegel",
			Namespace: "spegel",
			Labels: map[string]string{
				"helm.toolkit.fluxcd.io/name":      "spegel",
				"helm.toolkit.fluxcd.io/namespace": "spegel",
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
				},
			},
		},
	}
	spegelHR := &unstructured.Unstructured{}
	spegelHR.SetGroupVersionKind(fluxHelmReleaseGVK)
	spegelHR.SetNamespace("spegel")
	spegelHR.SetName("spegel")
	_ = unstructured.SetNestedField(spegelHR.Object, "spegel", "spec", "chart", "spec", "chart")

	// 2. NginxProxy hilton-public-proxy managed by Flux Kustomization flux-system/apps
	np := &unstructured.Unstructured{}
	np.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.nginx.org", Version: "v1alpha2", Kind: "NginxProxy"})
	np.SetNamespace("nginx-gateway")
	np.SetName("hilton-public-proxy")
	np.SetLabels(map[string]string{
		"kustomize.toolkit.fluxcd.io/name":      "apps",
		"kustomize.toolkit.fluxcd.io/namespace": "flux-system",
	})
	_ = unstructured.SetNestedSlice(np.Object, []interface{}{
		map[string]interface{}{"containerPort": int64(80), "port": int64(80)},
	}, "spec", "kubernetes", "deployment", "container", "hostPorts")

	appsKS := &unstructured.Unstructured{}
	appsKS.SetGroupVersionKind(fluxKustomizationGVK)
	appsKS.SetNamespace("flux-system")
	appsKS.SetName("apps")
	_ = unstructured.SetNestedField(appsKS.Object, "./apps/hilton", "spec", "path")

	// 3. Single-node hostPort Deployment managed by ArgoCD Application argocd/edge-gateway
	argoDep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "edge-gateway",
			Namespace: "default",
			Labels: map[string]string{
				"argocd.argoproj.io/instance": "edge-gateway",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{platform.RoleLabel: platform.RoleValue},
					Containers: []corev1.Container{{
						Name:  "proxy",
						Ports: []corev1.ContainerPort{{ContainerPort: 443, HostPort: 443}},
					}},
				},
			},
		},
	}
	argoApp := &unstructured.Unstructured{}
	argoApp.SetGroupVersionKind(argoApplicationGVK)
	argoApp.SetNamespace("argocd")
	argoApp.SetName("edge-gateway")

	c := gitopsTestClient(t, spegelDS, spegelHR, np, appsKS, argoDep, argoApp)

	// Before reconciliation, CheckGitOps* reports warnings when upstream CRs lack patches
	if w := CheckGitOpsDaemonSetExclusion(ctx, c, spegelDS, platform.RoleLabel, platform.RoleValue); len(w) == 0 {
		t.Fatal("expected warning for unpatched Flux HelmRelease spegel/spegel")
	}
	if w := CheckGitOpsWorkloadRecreate(ctx, c, np, "NginxProxy", "/spec/kubernetes/deployment/patches"); len(w) == 0 {
		t.Fatal("expected warning for unpatched Flux Kustomization flux-system/apps")
	}
	if w := CheckGitOpsWorkloadRecreate(ctx, c, argoDep, "Deployment", "/spec/strategy"); len(w) == 0 {
		t.Fatal("expected warning for unpatched ArgoCD Application argocd/edge-gateway")
	}

	report, err := ReconcileCalicoSite(ctx, c, c, 1420)
	if err != nil {
		t.Fatalf("ReconcileCalicoSite: %v", err)
	}
	if !report.GitOpsPatched || !report.ExclusionsPatched || !report.WorkloadsPatched {
		t.Fatalf("unexpected report flags: %+v", report)
	}

	// Verify upstream CRs now pass CheckGitOps* with zero warnings
	if w := CheckGitOpsDaemonSetExclusion(ctx, c, spegelDS, platform.RoleLabel, platform.RoleValue); len(w) != 0 {
		t.Fatalf("unexpected warning after HelmRelease patch: %v", w)
	}
	if w := CheckGitOpsWorkloadRecreate(ctx, c, np, "NginxProxy", "/spec/kubernetes/deployment/patches"); len(w) != 0 {
		t.Fatalf("unexpected warning after Kustomization patch: %v", w)
	}
	if w := CheckGitOpsWorkloadRecreate(ctx, c, argoDep, "Deployment", "/spec/strategy"); len(w) != 0 {
		t.Fatalf("unexpected warning after ArgoCD Application patch: %v", w)
	}
}

func TestReconcileGitOpsManifestFiles(t *testing.T) {
	dir := t.TempDir()
	npFile := filepath.Join(dir, "nginxproxy.yaml")
	hrFile := filepath.Join(dir, "spegel.yaml")

	if err := os.WriteFile(npFile, []byte(`apiVersion: gateway.nginx.org/v1alpha2
kind: NginxProxy
metadata:
  name: public-proxy
  namespace: nginx-gateway
spec:
  kubernetes:
    deployment:
      container:
        hostPorts:
          - containerPort: 80
            port: 80
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hrFile, []byte(`apiVersion: v1
kind: Namespace
metadata:
  name: spegel
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: spegel
  namespace: spegel
spec:
  chart:
    spec:
      chart: spegel
  values:
    nodeSelector:
      kubernetes.io/os: linux
`), 0o644); err != nil {
		t.Fatal(err)
	}

	changes, err := ReconcileGitOpsManifestFiles([]string{dir}, false)
	if err != nil {
		t.Fatalf("ReconcileGitOpsManifestFiles: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("expected 2 manifest file changes, got %v", changes)
	}
	npRaw, _ := os.ReadFile(npFile)
	if !strings.Contains(string(npRaw), "Recreate") {
		t.Fatalf("expected Recreate in patched %s:\n%s", npFile, string(npRaw))
	}
	hrRaw, _ := os.ReadFile(hrFile)
	if !strings.Contains(string(hrRaw), platform.RoleValue) {
		t.Fatalf("expected %s in patched %s:\n%s", platform.RoleValue, hrFile, string(hrRaw))
	}
}
