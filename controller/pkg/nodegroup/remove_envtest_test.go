package nodegroup

import (
	"context"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func verifyRemovalAfterWithdrawal(t *testing.T, api client.Client, gvk schema.GroupVersionKind, g *v1alpha1.ProvisionedNodeGroupClaim, child *v1alpha1.ProvisionedNodeClaim) {
	t.Helper()
	ctx := context.Background()
	if done, err := ResumeRemoval(ctx, api, api, gvk, g); err == nil || done {
		t.Fatal("uncommitted teardown accepted", done, err)
	}
	if done, err := BeginRemoval(ctx, api, gvk, g, "", "6443"); err != nil || !done {
		t.Fatal("acknowledged removal not committed", done, err)
	}
	stale := g.DeepCopy()
	if err := api.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
		t.Fatal(err)
	}
	if !g.Status.PendingAction.Removing {
		t.Fatal("API lost removal phase")
	}
	if done, err := ResumeRemoval(ctx, api, api, gvk, g); err != nil || done {
		t.Fatal("teardown completed on delete request", done, err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil {
		t.Fatal(err)
	}
	if child.DeletionTimestamp.IsZero() || len(child.Finalizers) != 1 {
		t.Fatal("claim finalizer bypassed")
	}
	if done, err := ResumeRemoval(ctx, api, api, gvk, g); err != nil || done {
		t.Fatal("terminating child was skipped", done, err)
	}
	// No CAPI controller runs in envtest. Release each resource explicitly to
	// verify waiting boundaries; real infrastructure teardown remains a VM gate.
	child.Finalizers = nil
	if err := api.Update(ctx, child); err != nil {
		t.Fatal(err)
	}
	if done, err := ResumeRemoval(ctx, api, api, gvk, g); err != nil || done {
		t.Fatal("live Machine was skipped", done, err)
	}
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(gvk)
	if err := api.Get(ctx, client.ObjectKeyFromObject(child), m); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(ctx, m); err != nil {
		t.Fatal(err)
	}
	replacement := &unstructured.Unstructured{}
	replacement.SetGroupVersionKind(gvk)
	replacement.SetName(m.GetName())
	replacement.SetNamespace(m.GetNamespace())
	if err := api.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if done, err := ResumeRemoval(ctx, api, api, gvk, g); err == nil || done {
		t.Fatal("replacement Machine accepted", done, err)
	}
	still := &unstructured.Unstructured{}
	still.SetGroupVersionKind(gvk)
	if err := api.Get(ctx, client.ObjectKeyFromObject(replacement), still); err != nil || still.GetUID() != replacement.GetUID() || !still.GetDeletionTimestamp().IsZero() {
		t.Fatal("replacement Machine modified", err)
	}
	if err := api.Delete(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if done, err := ResumeRemoval(ctx, api, api, gvk, g); err != nil || done {
		t.Fatal("live Node was skipped", done, err)
	}
	node := &corev1.Node{}
	if err := api.Get(ctx, client.ObjectKey{Name: g.Status.PendingAction.NodeName}, node); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
		t.Fatal(err)
	}
	if done, err := ResumeRemoval(ctx, api, api, gvk, g); err != nil || !done {
		t.Fatal("absent resources did not complete removal", done, err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
		t.Fatal(err)
	}
	if g.Status.PendingAction != nil {
		t.Fatal("completed action retained")
	}
	if done, err := BeginRemoval(ctx, api, gvk, stale, "", "6443"); err == nil || done {
		t.Fatal("stale action replayed", done, err)
	}
}
