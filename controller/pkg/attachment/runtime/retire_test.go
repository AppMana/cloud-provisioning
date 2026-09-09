package runtime

import (
	"context"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
)

func TestRetirementWaitsForDurableCompletionAndRequestRelease(t *testing.T) {
	ctx := context.Background()
	lifetime, record := lifetimeFixture(t)
	api := lifetime.Client
	store := attachment.ConfigMapStore{Client: api, Namespace: "test"}
	if _, err := RetireWorkerRequest(ctx, api, "test", "mesh", "worker", "first", record.Plan.Worker); err == nil {
		t.Fatal("missing journal authorized retirement")
	}
	if err := store.Save(ctx, nil, &record); err != nil {
		t.Fatal(err)
	}
	other := record.Plan.Worker
	other.NodeUID = "replacement"
	if _, err := RetireWorkerRequest(ctx, api, "test", "mesh", "worker", "first", other); err == nil {
		t.Fatal("replacement worker authorized retirement")
	}
	if _, err := RetireWorkerRequest(ctx, api, "test", "other-mesh", "worker", "first", record.Plan.Worker); err == nil {
		t.Fatal("foreign mesh authorized retirement")
	}
	if done, err := RetireWorkerRequest(ctx, api, "test", "mesh", "worker", "first", record.Plan.Worker); err != nil || done {
		t.Fatal(done, err)
	}
	cm := &corev1.ConfigMap{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: "test", Name: "worker"}, cm); err != nil {
		t.Fatal(err)
	}
	if cm.DeletionTimestamp.IsZero() || len(cm.Finalizers) == 0 {
		t.Fatal("retirement lost its finalizer")
	}
	current, err := store.Load(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	complete := *current
	complete.Phase = attachment.Complete
	if err := store.Save(ctx, current, &complete); err != nil {
		t.Fatal(err)
	}
	// Completion can precede successful CAPI hook release. The request remains
	// protected during that interval and must still prevent node teardown.
	if done, err := RetireWorkerRequest(ctx, api, "test", "mesh", "worker", "first", record.Plan.Worker); err != nil || done {
		t.Fatal("ignored outstanding request cleanup", done, err)
	}
	cm.Finalizers = nil
	if err := api.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if done, err := RetireWorkerRequest(ctx, api, "test", "mesh", "worker", "first", record.Plan.Worker); err != nil || !done {
		t.Fatal("completed retirement not observed", done, err)
	}
	// A same-name replacement must remain untouched even when the old journal
	// is complete; its UID establishes a separate lifecycle.
	cm.ResourceVersion = ""
	cm.DeletionTimestamp = nil
	cm.UID = "replacement-request"
	if err := api.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireWorkerRequest(ctx, api, "test", "mesh", "worker", "first", record.Plan.Worker); err == nil {
		t.Fatal("adopted replacement request")
	}
}
