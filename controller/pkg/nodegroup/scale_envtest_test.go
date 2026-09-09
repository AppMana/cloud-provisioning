package nodegroup

import (
	"context"
	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	env := &envtest.Environment{CRDDirectoryPaths: []string{"testdata", "../../../charts/cloud-provisioning/crds"}, ErrorIfCRDPathMissing: true}
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
	// Controller intent uses the same resource version as KEDA scale updates.
	scheme := runtime.NewScheme()
	if e = v1alpha1.AddToScheme(scheme); e != nil {
		t.Fatal(e)
	}
	if e = corev1.AddToScheme(scheme); e != nil {
		t.Fatal(e)
	}
	if e = policyv1.AddToScheme(scheme); e != nil {
		t.Fatal(e)
	}
	typed, e := client.New(cfg, client.Options{Scheme: scheme})
	if e != nil {
		t.Fatal(e)
	}
	observed := &v1alpha1.ProvisionedNodeGroupClaim{}
	key := types.NamespacedName{Namespace: "default", Name: created.GetName()}
	if e = typed.Get(ctx, key, observed); e != nil {
		t.Fatal(e)
	}
	action, e := ProposeAction(observed, nil)
	if e != nil || action == nil {
		t.Fatal(action, e)
	}
	staleGroup := observed.DeepCopy()
	committed, e := ReserveAction(ctx, typed, observed, action)
	if e != nil {
		t.Fatal(e)
	}
	if observed.Status.PendingAction != nil {
		t.Fatal("reservation mutated cached input")
	}
	if _, e = ReserveAction(ctx, typed, staleGroup, action); !apierrors.IsConflict(e) {
		t.Fatalf("concurrent reservation accepted: %v", e)
	}
	restarted := &v1alpha1.ProvisionedNodeGroupClaim{}
	if e = typed.Get(ctx, key, restarted); e != nil {
		t.Fatal(e)
	}
	if restarted.Status.PendingAction == nil || restarted.Status.PendingAction.ID != committed.Status.PendingAction.ID || restarted.Status.PendingAction.Template.Spec.InfrastructureRef.Name != "gpu" {
		t.Fatal("restart lost frozen intent")
	}
	if _, e = ProposeAction(restarted, nil); e == nil {
		t.Fatal("proposed duplicate while action pending")
	}
	// A scale event can alter desired capacity while the original operation is
	// outstanding; it must retain the operation for explicit resume/cancellation.
	scale, e = groups.Get(ctx, created.GetName(), metav1.GetOptions{}, "scale")
	if e != nil {
		t.Fatal(e)
	}
	_ = unstructured.SetNestedField(scale.Object, int64(0), "spec", "replicas")
	if _, e = groups.Update(ctx, scale, metav1.UpdateOptions{}, "scale"); e != nil {
		t.Fatal(e)
	}
	if e = typed.Get(ctx, key, restarted); e != nil {
		t.Fatal(e)
	}
	if *restarted.Spec.Replicas != 0 || restarted.Status.PendingAction.ID != action.ID {
		t.Fatal("scale update rewrote pending operation")
	}

	// Resume after a scale and template update using the original reservation.
	restarted.Spec.Template.Spec.InfrastructureRef.Name = "next-image"
	if e = typed.Update(ctx, restarted); e != nil {
		t.Fatal(e)
	}
	child, e := ResumeCreation(ctx, typed, key, restarted.UID, action.ID)
	if e != nil {
		t.Fatal(e)
	}
	if child.Spec.InfrastructureRef.Name != "gpu" || child.UID == "" {
		t.Fatal("resume lost frozen intent", child)
	}
	// A fresh client simulates losing the create response and restarting.
	fresh, e := client.New(cfg, client.Options{Scheme: scheme})
	if e != nil {
		t.Fatal(e)
	}
	retried, e := ResumeCreation(ctx, fresh, key, restarted.UID, action.ID)
	if e != nil || retried.UID != child.UID {
		t.Fatal("retry duplicated reserved child", retried, e)
	}
	claims := &v1alpha1.ProvisionedNodeClaimList{}
	if e = fresh.List(ctx, claims, client.InNamespace(key.Namespace)); e != nil {
		t.Fatal(e)
	}
	if len(claims.Items) != 1 {
		t.Fatal("duplicate capacity", len(claims.Items))
	}
	if _, e = ResumeCreation(ctx, fresh, key, "recreated-group", action.ID); e == nil {
		t.Fatal("wrong group UID authorized create")
	}
	if _, e = ResumeCreation(ctx, fresh, key, restarted.UID, "stale-action"); e == nil {
		t.Fatal("wrong action authorized create")
	}
	// A same-name object without the reserved action must never be adopted or overwritten.
	child.Annotations[ActionAnnotation] = "other-action"
	if e = fresh.Update(ctx, child); e != nil {
		t.Fatal(e)
	}
	if _, e = ResumeCreation(ctx, fresh, key, restarted.UID, action.ID); e == nil {
		t.Fatal("adopted conflicting action")
	}
	unchanged := &v1alpha1.ProvisionedNodeClaim{}
	if e = fresh.Get(ctx, client.ObjectKeyFromObject(child), unchanged); e != nil {
		t.Fatal(e)
	}
	if unchanged.Annotations[ActionAnnotation] != "other-action" {
		t.Fatal("rewrote conflicting child")
	}

	if _, e = CompleteCreation(ctx, fresh, restarted, child.UID); e == nil {
		t.Fatal("completed mismatched action")
	}
	child.Annotations[ActionAnnotation] = action.ID
	if e = fresh.Update(ctx, child); e != nil {
		t.Fatal(e)
	}
	if _, e = CompleteCreation(ctx, fresh, restarted, "replaced-child"); e == nil {
		t.Fatal("completed replacement child")
	}
	if e = fresh.Get(ctx, key, restarted); e != nil {
		t.Fatal(e)
	}
	staleCompletion := restarted.DeepCopy()
	// Race completion with another scaler update.
	restarted.Spec.Replicas = new(int32)
	*restarted.Spec.Replicas = 3
	if e = fresh.Update(ctx, restarted); e != nil {
		t.Fatal(e)
	}
	if _, e = CompleteCreation(ctx, fresh, staleCompletion, child.UID); !apierrors.IsConflict(e) {
		t.Fatalf("stale completion accepted: %v", e)
	}
	if e = fresh.Get(ctx, key, restarted); e != nil {
		t.Fatal(e)
	}
	completed, e := CompleteCreation(ctx, fresh, restarted, child.UID)
	if e != nil {
		t.Fatal(e)
	}
	if completed.Status.PendingAction != nil || restarted.Status.PendingAction == nil || *completed.Spec.Replicas != 3 {
		t.Fatal("completion changed caller or desired capacity")
	}
	if _, e = ResumeCreation(ctx, fresh, key, restarted.UID, action.ID); e == nil {
		t.Fatal("replayed completed action")
	}
	next, e := ProposeAction(completed, []v1alpha1.ProvisionedNodeClaim{*child})
	if e != nil || next == nil || next.Ordinal != 1 || next.Template.Spec.InfrastructureRef.Name != "next-image" {
		t.Fatal("next replica did not use current template", next, e)
	}
	reservedNext, e := ReserveAction(ctx, fresh, completed, next)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = CompleteCreation(ctx, fresh, reservedNext, child.UID); !apierrors.IsNotFound(e) {
		t.Fatalf("completed absent child: %v", e)
	}
	if _, e = CompleteCreation(ctx, fresh, restarted, child.UID); !apierrors.IsConflict(e) {
		t.Fatalf("old completion cleared new reservation: %v", e)
	}
	if e = fresh.Get(ctx, key, restarted); e != nil {
		t.Fatal(e)
	}
	if restarted.Status.PendingAction.ID != next.ID {
		t.Fatal("lost next reservation")
	}

	verifyRealAPIDrain(t, fresh)
	verifyRealAPIReconciliation(t, fresh)
	verifyDeletingGroupCreation(t, fresh)
	verifySerialGatewayRetirement(t, fresh)
	verifyPeerWithdrawalCapture(t, fresh)

}
