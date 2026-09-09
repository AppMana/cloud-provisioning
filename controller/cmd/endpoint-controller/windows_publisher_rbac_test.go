package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Exercise actual chart RBAC with the controller identity. Admin-only Pod
// admission and fake-client reconciliation did not catch a denied live patch.
func TestWindowsPublisherReconcilesWithChartServiceAccount(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for isolated API authorization")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm is required to render actual chart RBAC")
	}
	ctx := context.Background()
	const namespace = "publisher-test"
	const release = "cloud-provisioning"
	rendered, err := exec.CommandContext(ctx, "helm", "template", release, "../../../charts/cloud-provisioning", "--namespace", namespace, "--show-only", "templates/rbac.yaml").Output()
	if err != nil {
		t.Fatal(err)
	}
	e := &envtest.Environment{}
	e.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	config, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	admin, err := client.New(config, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(&object.Object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if (object.GetNamespace() != namespace && object.GetNamespace() != "") || object.GetName() != release+"-endpoint-controller" {
			continue
		}
		switch object.GetKind() {
		case "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding":
			if err := admin.Create(ctx, object); err != nil {
				t.Fatal(err)
			}
		}
	}
	identity := rest.CopyConfig(config)
	identity.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + namespace + ":" + release + "-endpoint-controller", Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"}}
	scoped, err := client.New(identity, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := windowsPublisherFixture()
	r.dialerCloudDaemonSetName = release + "-dialer-remote"
	r.Client = scoped
	r.reader = scoped
	if err := r.ensureWindowsPublisherDaemonSet(ctx); err != nil {
		t.Fatal("create with controller identity:", err)
	}
	r.windowsPublisherImage = "example.invalid/publisher@sha256:" + strings.Repeat("b", 64)
	if err := r.ensureWindowsPublisherDaemonSet(ctx); err != nil {
		t.Fatal("patch with controller identity:", err)
	}
	t.Run("attachment withdrawal is namespace confined", func(t *testing.T) {
		own := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gateway-request", Namespace: namespace}}
		if err := admin.Create(ctx, own); err != nil {
			t.Fatal(err)
		}
		if err := scoped.Delete(ctx, own); err != nil {
			t.Fatal("automatic withdrawal cannot delete request", err)
		}
		const otherNamespace = "other-mesh"
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: otherNamespace}}); err != nil {
			t.Fatal(err)
		}
		other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gateway-request", Namespace: otherNamespace}}
		if err := admin.Create(ctx, other); err != nil {
			t.Fatal(err)
		}
		if err := scoped.Delete(ctx, other); !apierrors.IsForbidden(err) {
			t.Fatal("request deletion escaped namespace", err)
		}
	})
}
