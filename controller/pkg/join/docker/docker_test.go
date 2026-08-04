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

func TestInfraValues_ArchIsTheHosts(t *testing.T) {
	values, err := Provider{}.InfraValues(context.Background(), &unstructured.Unstructured{})
	if err != nil {
		t.Fatalf("InfraValues: %v", err)
	}
	if values["arch"] != runtime.GOARCH {
		t.Errorf("arch = %v, want the host's %s", values["arch"], runtime.GOARCH)
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
