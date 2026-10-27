package cni

import (
	"context"
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

// k0s writes the MTU into the CNI configuration itself; Calico's own
// manifests template it from CNI_MTU, usually a calico-config key.
func TestCalicoPodMTUReadsEitherForm(t *testing.T) {
	raw, err := os.ReadFile("site/testdata/k0s-v1.36.5-calico-node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	k0s := &appsv1.DaemonSet{}
	if err := yaml.Unmarshal(raw, k0s); err != nil {
		t.Fatal(err)
	}
	if mtu, _, err := CalicoPodMTU(context.Background(), nil, k0s); err != nil || mtu != 1450 {
		t.Fatalf("k0s mtu %d, %v", mtu, err)
	}

	templated := k0s.DeepCopy()
	for i, c := range templated.Spec.Template.Spec.InitContainers {
		for j, e := range c.Env {
			if e.Name == "CNI_NETWORK_CONFIG" {
				templated.Spec.Template.Spec.InitContainers[i].Env[j].Value = `{"plugins":[{"type":"calico","mtu":"__CNI_MTU__"}]}`
				templated.Spec.Template.Spec.InitContainers[i].Env = append(templated.Spec.Template.Spec.InitContainers[i].Env, corev1.EnvVar{
					Name: "CNI_MTU", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "calico-config"}, Key: "veth_mtu"}}})
			}
		}
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "calico-config", Namespace: "kube-system"}, Data: map[string]string{"veth_mtu": "1370"}}).Build()
	if mtu, source, err := CalicoPodMTU(context.Background(), c, templated); err != nil || mtu != 1370 || source != "CNI_MTU" {
		t.Fatalf("templated mtu %d from %q, %v", mtu, source, err)
	}
}
