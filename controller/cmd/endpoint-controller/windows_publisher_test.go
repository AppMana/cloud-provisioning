package main

import (
	"context"
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func windowsPublisherFixture() *meshReconciler {
	return &meshReconciler{secretNamespace: "publisher-test", dialerCloudDaemonSetName: "remote", dialerServiceAccount: "dialer", ifaceName: "cldt1234abcd", windowsPublisherImage: "example.invalid/publisher@sha256:" + strings.Repeat("a", 64)}
}

func TestWindowsPublisherUsesNativeHostStateAndProjectedCredentials(t *testing.T) {
	r := windowsPublisherFixture()
	d := r.windowsPublisherDaemonSet()
	p := d.Spec.Template.Spec
	if p.OS.Name != corev1.Windows || p.NodeSelector[corev1.LabelOSStable] != "windows" || p.NodeSelector[cloudWorkerRoleLabel] != cloudWorkerRoleValue {
		t.Fatal("publisher could schedule onto wrong node type")
	}
	if !p.HostNetwork || p.SecurityContext.WindowsOptions.HostProcess == nil || !*p.SecurityContext.WindowsOptions.HostProcess {
		t.Fatal("publisher is not HostProcess")
	}
	if p.AutomountServiceAccountToken == nil || *p.AutomountServiceAccountToken || p.ServiceAccountName != "dialer" {
		t.Fatal("unexpected credential source")
	}
	var projected, host bool
	for _, v := range p.Volumes {
		if v.HostPath != nil {
			host = v.HostPath.Path == `C:\ProgramData\CloudProvisioning\cldt1234abcd` && *v.HostPath.Type == corev1.HostPathDirectory
		}
		if v.Projected != nil {
			for _, source := range v.Projected.Sources {
				if source.ServiceAccountToken != nil {
					projected = *source.ServiceAccountToken.ExpirationSeconds == 3600
				}
			}
		}
	}
	if !projected || !host {
		t.Fatal("missing renewable credentials or pre-existing host state")
	}
	args := strings.Join(p.Containers[0].Args, " ")
	if strings.Contains(args, "peers-file") || !strings.Contains(args, "request-file") || !strings.Contains(args, "receipt-file") {
		t.Fatal("publisher protocol paths are incorrect")
	}
}

func TestWindowsPublisherReconcilesImageUpdates(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := windowsPublisherFixture()
	r.Client = c
	r.reader = c
	if err := r.ensureWindowsPublisherDaemonSet(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.windowsPublisherImage = "example.invalid/publisher@sha256:" + strings.Repeat("b", 64)
	r.windowsPublisherAPIServer = "https://[::1]:7443"
	if err := r.ensureWindowsPublisherDaemonSet(context.Background()); err != nil {
		t.Fatal(err)
	}
	actual := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: r.secretNamespace, Name: "remote-windows"}, actual); err != nil {
		t.Fatal(err)
	}
	if actual.Spec.Template.Spec.Containers[0].Image != r.windowsPublisherImage {
		t.Fatal("image update not reconciled")
	}
	if !strings.Contains(strings.Join(actual.Spec.Template.Spec.Containers[0].Args, " "), "--api-server=https://[::1]:7443") {
		t.Fatal("host-reachable API endpoint update not reconciled")
	}
}

func TestWindowsPublisherPassesKubernetesAdmission(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for isolated API admission")
	}
	e := &envtest.Environment{}
	config, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	c, err := client.New(config, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := windowsPublisherFixture()
	ctx := context.Background()
	if err = c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: r.secretNamespace}}); err != nil {
		t.Fatal(err)
	}
	if err = c.Create(ctx, r.windowsPublisherDaemonSet()); err != nil {
		t.Fatal(err)
	}
	// Admit the actual Pod too: DaemonSet validation alone does not exercise all
	// Pod admission checks, including the service-account mutation/validation.
	if err = c.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: r.dialerServiceAccount, Namespace: r.secretNamespace}}); err != nil {
		t.Fatal(err)
	}
	template := r.windowsPublisherDaemonSet().Spec.Template
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "publisher", Namespace: r.secretNamespace}, Spec: template.Spec}
	if err = c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
}
