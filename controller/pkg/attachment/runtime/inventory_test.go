package runtime

import (
	"context"
	"encoding/json"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
)

func TestWorkerInventoryRetainsRetiringRequestsAndRejectsIdentityDrift(t *testing.T) {
	ctx := context.Background()
	lifetime, record := lifetimeFixture(t)
	api := lifetime.Client
	refs, err := WorkerRequests(ctx, api, "test", "mesh", record.Plan.Worker)
	if err != nil || len(refs) != 1 || refs[0].UID != "first" {
		t.Fatal(refs, err)
	}
	original := &corev1.ConfigMap{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: "test", Name: "worker"}, original); err != nil {
		t.Fatal(err)
	}
	second := original.DeepCopy()
	second.Name = "another-request"
	second.UID = "second"
	second.ResourceVersion = ""
	if err := api.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	// Label withdrawal still leaves an owned request requiring retirement.
	original.Labels = nil
	// Label-based retirement may already have released the finalizer while
	// retaining the ConfigMap. Its identity still belongs in the inventory.
	original.Finalizers = nil
	if err := api.Update(ctx, original); err != nil {
		t.Fatal(err)
	}
	refs, err = WorkerRequests(ctx, api, "test", "mesh", record.Plan.Worker)
	if err != nil || len(refs) != 2 || refs[0].Name != "another-request" || refs[1].UID != "first" {
		t.Fatal("incomplete or unstable inventory", refs, err)
	}
	if refs, err := WorkerRequests(ctx, api, "test", "other-mesh", record.Plan.Worker); err != nil || len(refs) != 0 {
		t.Fatal("cross-mesh inventory", refs, err)
	}
	if _, err := WorkerRequests(ctx, api, "test", "mesh", record.Plan.Gateway); err == nil {
		t.Fatal("shared gateway treated as disposable worker")
	}
	var request attachment.GatewayRequest
	if err := json.Unmarshal([]byte(second.Data["request.json"]), &request); err != nil {
		t.Fatal(err)
	}
	request.Worker.NodeUID = "replacement-node"
	raw, _ := json.Marshal(request)
	second.Data["request.json"] = string(raw)
	if err := api.Update(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := WorkerRequests(ctx, api, "test", "mesh", record.Plan.Worker); err == nil {
		t.Fatal("ignored worker identity drift")
	}
}
