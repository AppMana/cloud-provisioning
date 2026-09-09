package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func lifetimeFixture(t *testing.T) (CAPILifetime, attachment.Record) {
	t.Helper()
	r, _, req := requestFixture(t)
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, req.NamespacedName, cm); err != nil {
		t.Fatal(err)
	}
	var desired attachment.GatewayRequest
	if err := json.Unmarshal([]byte(cm.Data["request.json"]), &desired); err != nil {
		t.Fatal(err)
	}
	plan, err := attachment.PlanGateway(desired)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []attachment.Machine{plan.Worker, plan.Gateway} {
		obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cluster.x-k8s.io/v1beta2", "kind": "Machine", "metadata": map[string]any{"name": m.UID, "namespace": "test", "uid": m.UID, "finalizers": []any{"test/keep"}}, "spec": map[string]any{"providerID": m.ProviderID, "clusterName": "cluster"}, "status": map[string]any{"nodeRef": map[string]any{"name": m.UID}}}}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: m.UID, UID: types.UID(m.NodeUID)}, Spec: corev1.NodeSpec{ProviderID: m.ProviderID}}
		for _, object := range []client.Object{obj, node} {
			if err := r.Client.Create(ctx, object); err != nil {
				t.Fatal(err)
			}
		}
	}
	return CAPILifetime{Client: r.Client, Namespace: "test", ClusterName: "cluster", MeshName: "mesh"}, attachment.Record{ID: "test/worker/first", Lease: "epoch", Digest: "digest", Plan: plan, Phase: attachment.Preparing}
}

func TestCAPILifetimeAutomaticallyWithdrawsDeletingParticipant(t *testing.T) {
	ctx := context.Background()
	c, record := lifetimeFixture(t)
	if deleting, err := c.Protect(ctx, record); err != nil || deleting {
		t.Fatal(deleting, err)
	}
	machines, _ := c.machines(ctx)
	for _, machine := range machines {
		if !attachment.HasDeletionHold(machine) {
			t.Fatal("participant not protected")
		}
	}
	if err := c.Client.Delete(ctx, machines[record.Plan.Worker.UID]); err != nil {
		t.Fatal(err)
	}
	if deleting, err := c.Protect(ctx, record); err != nil || !deleting {
		t.Fatal(deleting, err)
	}
	cm, err := c.request(ctx, record)
	if err != nil || cm.DeletionTimestamp == nil {
		t.Fatal("request did not enter durable withdrawal", err)
	}
	machines, _ = c.machines(ctx)
	if !attachment.HasDeletionHold(machines[record.Plan.Worker.UID]) {
		t.Fatal("deletion released holds before withdrawal")
	}
}

func TestCAPILifetimeReleasePreservesSharedLeaseAndReplacement(t *testing.T) {
	ctx := context.Background()
	c, first := lifetimeFixture(t)
	second := first
	second.Lease = "second"
	for _, r := range []attachment.Record{first, second} {
		if _, err := c.Protect(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Release(ctx, first); err != nil {
		t.Fatal(err)
	}
	machines, _ := c.machines(ctx)
	drain, terminate := attachment.DeletionHookKeys(first.LeaseID())
	otherDrain, otherTerminate := attachment.DeletionHookKeys(second.LeaseID())
	for _, m := range machines {
		a := m.GetAnnotations()
		if a[drain] != "" || a[terminate] != "" || a[otherDrain] != second.LeaseID() || a[otherTerminate] != second.LeaseID() {
			t.Fatal("shared lease changed")
		}
	}
	// A same-name replacement cannot inherit or lose the old UID's hook pair.
	replacement := machines[first.Plan.Worker.UID].DeepCopy()
	old := machines[first.Plan.Worker.UID]
	old.SetFinalizers(nil)
	if err := c.Client.Update(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := c.Client.Delete(ctx, old); err != nil {
		t.Fatal(err)
	}
	replacement.SetUID("replacement")
	replacement.SetResourceVersion("")
	if err := c.Client.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := c.Release(ctx, second); err != nil {
		t.Fatal(err)
	}
	current, _ := c.machines(ctx)
	if current["replacement"].GetAnnotations()[otherDrain] != second.LeaseID() {
		t.Fatal("touched replacement UID")
	}
}

func TestCAPILifetimeRejectsChangedRequestAndForeignParticipant(t *testing.T) {
	ctx := context.Background()
	c, r := lifetimeFixture(t)
	wrong := r
	wrong.ID = "test/worker/replacement"
	if _, err := c.Protect(ctx, wrong); err == nil {
		t.Fatal("adopted replacement request")
	}
	r.Plan.Worker.ProviderID = "foreign"
	if _, err := c.Protect(ctx, r); err == nil {
		t.Fatal("protected foreign provider")
	}
	machines, _ := c.machines(ctx)
	for _, m := range machines {
		if attachment.HasDeletionHold(m) {
			t.Fatal("wrote hooks before validating all participants")
		}
	}
}
