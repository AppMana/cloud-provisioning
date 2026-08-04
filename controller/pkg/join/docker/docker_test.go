package docker

import (
	"context"
	"runtime"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResolveInstanceType_EverythingIsAContainer(t *testing.T) {
	got, err := Provider{}.ResolveInstanceType(join.NodeRequest{CPUMillis: 64000, MemoryBytes: 1 << 40})
	if err != nil || got != "docker" {
		t.Errorf("ResolveInstanceType = (%q, %v), want (docker, nil) -- containers have no instance types", got, err)
	}
}

func TestInfraValues_ArchIsTheHosts(t *testing.T) {
	values, err := Provider{}.InfraValues(context.Background(), &unstructured.Unstructured{})
	if err != nil {
		t.Fatalf("InfraValues: %v", err)
	}
	if values["arch"] != runtime.GOARCH {
		t.Errorf("arch = %v, want the host's %s", values["arch"], runtime.GOARCH)
	}
}

func TestInfraMachine_RendersDockerMachineFromConfig(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "docker-provider-config", Namespace: "wg-dialer"},
		Data:       map[string][]byte{"node-image": []byte("kindest/node:v1.33.1")},
	}).Build()
	p := Provider{ConfigNamespace: "wg-dialer", ConfigName: "docker-provider-config"}

	obj, err := p.InfraMachine(context.Background(), c, "default", "docker", join.NodeRequest{})
	if err != nil {
		t.Fatalf("InfraMachine: %v", err)
	}
	if obj.GroupVersionKind() != p.GVK() {
		t.Errorf("GVK = %v, want %v", obj.GroupVersionKind(), p.GVK())
	}
	image, _, _ := unstructured.NestedString(obj.Object, "spec", "customImage")
	if image != "kindest/node:v1.33.1" {
		t.Errorf("spec.customImage = %q, want the config's node-image", image)
	}
}

func TestInfraMachine_MissingNodeImage_IsAnError(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "docker-provider-config", Namespace: "wg-dialer"},
	}).Build()
	p := Provider{ConfigNamespace: "wg-dialer", ConfigName: "docker-provider-config"}

	if _, err := p.InfraMachine(context.Background(), c, "default", "docker", join.NodeRequest{}); err == nil {
		t.Fatal("expected an error when node-image is unset")
	}
}

func TestInfraMachine_ExtraMountsAndPreloadImages(t *testing.T) {
	// A local node container has no registry and no credentials, so
	// locally-built images reach it only via CAPD's preload; the
	// mounts are how a binary reaches it without a download server.
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "docker-provider-config", Namespace: "wg-dialer"},
		Data: map[string][]byte{
			"node-image":     []byte("kindest/node:v1.34.0"),
			"extra-mounts":   []byte("/host/dist:/opt/dialer-dist, /host/other:/opt/other"),
			"preload-images": []byte("cldt-dialer:e2e, other:tag"),
		},
	}).Build()
	p := Provider{ConfigNamespace: "wg-dialer", ConfigName: "docker-provider-config"}

	obj, err := p.InfraMachine(context.Background(), c, "default", "docker", join.NodeRequest{})
	if err != nil {
		t.Fatalf("InfraMachine: %v", err)
	}
	mounts, _, _ := unstructured.NestedSlice(obj.Object, "spec", "extraMounts")
	if len(mounts) != 2 {
		t.Fatalf("extraMounts = %v, want 2", mounts)
	}
	first := mounts[0].(map[string]any)
	if first["hostPath"] != "/host/dist" || first["containerPath"] != "/opt/dialer-dist" || first["readOnly"] != true {
		t.Errorf("first mount = %v, want /host/dist -> /opt/dialer-dist read-only", first)
	}
	images, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "preLoadImages")
	if len(images) != 2 || images[0] != "cldt-dialer:e2e" {
		t.Errorf("preLoadImages = %v, want both entries trimmed", images)
	}
}

func TestInfraMachine_MalformedExtraMount_IsAnError(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "docker-provider-config", Namespace: "wg-dialer"},
		Data: map[string][]byte{
			"node-image":   []byte("kindest/node:v1.34.0"),
			"extra-mounts": []byte("/host/dist"),
		},
	}).Build()
	p := Provider{ConfigNamespace: "wg-dialer", ConfigName: "docker-provider-config"}

	if _, err := p.InfraMachine(context.Background(), c, "default", "docker", join.NodeRequest{}); err == nil {
		t.Fatal("expected an error for an extra-mounts entry without a container path")
	}
}
