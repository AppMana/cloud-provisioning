package calico

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/network"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func preparedDaemonSet() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: "calico-node", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "calico-node", Image: "ghcr.io/appmana/node@sha256:" + strings.Repeat("a", 64),
				Env: []corev1.EnvVar{{Name: "CALICO_IPV4POOL_CIDR", Value: "192.168.0.0/16"}, {Name: "PRESERVE", Value: "literal"}}}},
		}}},
	}
}

func TestPreparedListUsesNativeObjectsAndPreservesCustomResources(t *testing.T) {
	custom := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "crd.projectcalico.org/v1", "kind": "IPAMConfig",
		"metadata": map[string]any{"name": "default"},
		"spec":     map[string]any{"strictAffinity": true, "futureField": map[string]any{"preserve": "literal"}},
	}}
	list := &corev1.List{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "List"}, Items: []runtime.RawExtension{
		{Object: preparedDaemonSet()}, {Object: custom},
	}}
	body, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "prepared.json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	objects, err := ReadObjects(context.Background(), path, fmt.Sprintf("%x", sha256.Sum256(body)))
	if err != nil {
		t.Fatal(err)
	}
	prepared, _, err := prepareObjects(objects, "10.244.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared[1].(*unstructured.Unstructured).Object, custom.Object) {
		t.Fatal("custom resource fields changed")
	}
	if _, err := ReadObjects(context.Background(), path, strings.Repeat("0", 64)); err == nil {
		t.Fatal("accepted wrong content pin")
	}
}

func TestNativePoolPinPreservesCallerAndImageIdentity(t *testing.T) {
	ds := preparedDaemonSet()
	before := ds.DeepCopy()
	objects, images, err := prepareObjects([]runtime.Object{ds}, "10.244.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, ds) {
		t.Fatal("mutated caller's native object")
	}
	pod := objects[0].(*appsv1.DaemonSet).Spec.Template.Spec
	if pod.Containers[0].ImagePullPolicy != corev1.PullNever {
		t.Fatal("runtime image pulls permitted")
	}
	if !reflect.DeepEqual(images, []string{ds.Spec.Template.Spec.Containers[0].Image}) {
		t.Fatal("digest identity lost", images)
	}
	env := pod.Containers[0].Env
	if len(env) != 2 || env[0].Name != "PRESERVE" || env[1].Name != "CALICO_IPV4POOL_CIDR" || env[1].Value != "10.244.0.0/16" {
		t.Fatalf("pool pin failed: %+v", env)
	}
}

func TestMissingObjectsFailBeforeAnyClusterAccess(t *testing.T) {
	if err := (Installer{}).Install(context.Background(), network.Deps{}); err == nil {
		t.Fatal("accepted missing fork objects")
	}
}

func TestInvalidObjectsFailClosed(t *testing.T) {
	for _, mode := range []string{"unpinned image", "wrong namespace", "missing container", "missing kind"} {
		t.Run(mode, func(t *testing.T) {
			ds := preparedDaemonSet()
			switch mode {
			case "unpinned image":
				ds.Spec.Template.Spec.Containers[0].Image = "upstream/node:latest"
			case "wrong namespace":
				ds.Namespace = "other"
			case "missing container":
				ds.Spec.Template.Spec.Containers[0].Name = "other"
			case "missing kind":
				ds.Kind = ""
			}
			if _, _, err := prepareObjects([]runtime.Object{ds}, "10.244.0.0/16"); err == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
}
