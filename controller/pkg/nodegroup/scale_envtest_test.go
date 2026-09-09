package nodegroup

import (
	"context"
	"os"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestRealAPIScaleContract(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for isolated API validation")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{"testdata"}, ErrorIfCRDPathMissing: true}
	cfg, e := env.Start()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := env.Stop(); e != nil {
			t.Error(e)
		}
	})
	c, e := dynamic.NewForConfig(cfg)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	groups := c.Resource(schema.GroupVersionResource{Group: "cloud-provisioning.appmana.com", Version: "v1alpha1", Resource: "provisionednodegroupclaims"}).Namespace("default")
	group := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cloud-provisioning.appmana.com/v1alpha1", "kind": "ProvisionedNodeGroupClaim",
		"metadata": map[string]interface{}{"name": "render-workers"},
		"spec":     map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"clusterName": "site", "infrastructureRef": map[string]interface{}{"apiGroup": "infrastructure.cluster.x-k8s.io", "kind": "AWSMachineTemplate", "name": "gpu"}}}},
	}}
	created, e := groups.Create(ctx, group, metav1.CreateOptions{})
	if e != nil {
		t.Fatal(e)
	}
	replicas := func(obj *unstructured.Unstructured, fields ...string) int64 {
		t.Helper()
		v, found, e := unstructured.NestedInt64(obj.Object, fields...)
		if e != nil || !found {
			t.Fatal(fields, found, e)
		}
		return v
	}
	if replicas(created, "spec", "replicas") != 1 {
		t.Fatal("missing replica default")
	}
	scale, e := groups.Get(ctx, created.GetName(), metav1.GetOptions{}, "scale")
	if e != nil {
		t.Fatal(e)
	}
	if scale.GetKind() != "Scale" || scale.GetAPIVersion() != "autoscaling/v1" || replicas(scale, "spec", "replicas") != 1 || replicas(scale, "status", "replicas") != 0 {
		t.Fatal("invalid scale discovery/defaults", scale)
	}
	created.Object["status"] = map[string]interface{}{"replicas": int64(2), "readyReplicas": int64(1), "selector": "app=render-workers"}
	if _, e = groups.UpdateStatus(ctx, created, metav1.UpdateOptions{}); e != nil {
		t.Fatal(e)
	}
	scale, e = groups.Get(ctx, created.GetName(), metav1.GetOptions{}, "scale")
	if e != nil {
		t.Fatal(e)
	}
	selector, _, _ := unstructured.NestedString(scale.Object, "status", "selector")
	if selector != "app=render-workers" || replicas(scale, "status", "replicas") != 2 {
		t.Fatal("scale status lost capacity or pod selector")
	}
	stale := scale.DeepCopy()
	_ = unstructured.SetNestedField(scale.Object, int64(3), "spec", "replicas")
	if _, e = groups.Update(ctx, scale, metav1.UpdateOptions{}, "scale"); e != nil {
		t.Fatal(e)
	}
	_ = unstructured.SetNestedField(stale.Object, int64(0), "spec", "replicas")
	if _, e = groups.Update(ctx, stale, metav1.UpdateOptions{}, "scale"); !apierrors.IsConflict(e) {
		t.Fatalf("stale scaler accepted: %v", e)
	}
	current, e := groups.Get(ctx, created.GetName(), metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if replicas(current, "spec", "replicas") != 3 || replicas(current, "status", "replicas") != 2 || replicas(current, "status", "readyReplicas") != 1 {
		t.Fatal("scaling changed observed status")
	}
	template, _, _ := unstructured.NestedString(current.Object, "spec", "template", "spec", "infrastructureRef", "name")
	if template != "gpu" {
		t.Fatal("scaling changed template")
	}
	for _, desired := range []int64{0, 5} {
		scale, e = groups.Get(ctx, created.GetName(), metav1.GetOptions{}, "scale")
		if e != nil {
			t.Fatal(e)
		}
		_ = unstructured.SetNestedField(scale.Object, desired, "spec", "replicas")
		if _, e = groups.Update(ctx, scale, metav1.UpdateOptions{}, "scale"); e != nil {
			t.Fatal(e)
		}
	}
	scale, e = groups.Get(ctx, created.GetName(), metav1.GetOptions{}, "scale")
	if e != nil {
		t.Fatal(e)
	}
	_ = unstructured.SetNestedField(scale.Object, int64(-1), "spec", "replicas")
	if _, e = groups.Update(ctx, scale, metav1.UpdateOptions{}, "scale"); !apierrors.IsInvalid(e) {
		t.Fatalf("negative replica target accepted: %v", e)
	}
}
