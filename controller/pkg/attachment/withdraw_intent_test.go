package attachment

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestRemoteWithdrawalRetainsUnavailableSurvivors(t *testing.T) {
	ctx := context.Background()
	r, record, _, _ := membershipFixture(t)
	api := r.Reader.(client.Client)
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"})
	if err := api.Get(ctx, client.ObjectKey{Namespace: "test", Name: "worker"}, m); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PrepareRemoteWithdrawal(ctx, "worker", record.Plan.Worker, "group/action"); err == nil {
		t.Fatal("unmarked worker accepted")
	}
	annotations := m.GetAnnotations()
	annotations[DrainIntentAnnotation] = "group/action"
	m.SetAnnotations(annotations)
	if err := api.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	intent, err := r.PrepareRemoteWithdrawal(ctx, "worker", record.Plan.Worker, "group/action")
	if err != nil {
		t.Fatal(err)
	}
	// Fixture Nodes have no Ready condition. They remain mandatory recipients.
	if len(intent.Consumers) != 2 || intent.Consumers[0].NodeName != "gateway" || intent.Consumers[1].NodeName != "other" {
		t.Fatalf("survivors: %+v", intent.Consumers)
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	var restored RemoteWithdrawalIntent
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(intent, &restored) {
		t.Fatal("restart lost recipient identity")
	}
	node := &corev1.Node{}
	if err := api.Get(ctx, client.ObjectKey{Name: "other"}, node); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(ctx, node); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PrepareRemoteWithdrawal(ctx, "worker", record.Plan.Worker, "group/action"); err == nil {
		t.Fatal("missing survivor silently omitted")
	}
	if len(restored.Consumers) != 2 {
		t.Fatal("retained inventory shrank")
	}
}

func TestRemoteWithdrawalRejectsChangedWorker(t *testing.T) {
	r, record, _, _ := membershipFixture(t)
	for _, worker := range []Machine{{UID: "replacement", NodeUID: record.Plan.Worker.NodeUID, ProviderID: record.Plan.Worker.ProviderID}, {}} {
		if _, err := r.PrepareRemoteWithdrawal(context.Background(), "worker", worker, "group/action"); err == nil {
			t.Fatal("invalid worker accepted")
		}
	}
}

type changingWithdrawalSource struct {
	client.Reader
	meshReads int
}

func (r *changingWithdrawalSource) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := r.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if key.Name == "mesh" {
		r.meshReads++
		if r.meshReads > 1 {
			obj.SetResourceVersion("changed")
		}
	}
	return nil
}
func TestRemoteWithdrawalRejectsConcurrentSourceChange(t *testing.T) {
	ctx := context.Background()
	r, record, _, _ := membershipFixture(t)
	api := r.Reader.(client.Client)
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"})
	if err := api.Get(ctx, client.ObjectKey{Namespace: "test", Name: "worker"}, m); err != nil {
		t.Fatal(err)
	}
	annotations := m.GetAnnotations()
	annotations[DrainIntentAnnotation] = "group/action"
	m.SetAnnotations(annotations)
	if err := api.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	r.Reader = &changingWithdrawalSource{Reader: api}
	if _, err := r.PrepareRemoteWithdrawal(ctx, "worker", record.Plan.Worker, "group/action"); err == nil {
		t.Fatal("changed mesh source accepted")
	}
}
