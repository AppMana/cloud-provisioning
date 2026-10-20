package handover

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestStoreRecheckExecutionLiveChanges(t *testing.T) {
	original := transitionFixture(t)
	baseStore, before := checkpointFixture(t, original)
	at := epoch.Add(time.Second)
	observed, err := original.Observe(at, receipts(t, view(t, original).Round, at)[:1])
	if err != nil {
		t.Fatal(err)
	}
	advanced := step(t, original, at)
	for _, name := range []string{
		"unchanged", "revision-only", "observations", "observations-same-revision",
		"recreated", "deleted", "deleting", "label-same-revision",
		"malformed-same-revision", "advance", "wrong-cluster", "wrong-expected-uid",
		"empty-snapshot",
	} {
		t.Run(name, func(t *testing.T) {
			api := fake.NewClientset(before.object.DeepCopy())
			store := baseStore
			store.ConfigMaps = api.CoreV1().ConfigMaps(before.object.Namespace)
			store.ExpectedUID = string(before.object.UID)
			prior := before
			live := before.object.DeepCopy()
			wantOK := false
			switch name {
			case "unchanged":
				wantOK = true
			case "revision-only":
				live.ResourceVersion = "new-revision"
				wantOK = true
			case "observations", "observations-same-revision":
				raw, err := observed.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				live.Data[journalKey] = string(raw)
				if name == "observations" {
					live.ResourceVersion = "new-revision"
				}
				wantOK = true
			case "recreated":
				live.UID = "replacement"
			case "deleted":
			case "deleting":
				now := metav1.Now()
				live.DeletionTimestamp = &now
			case "label-same-revision":
				live.Labels = map[string]string{"changed": "true"}
			case "malformed-same-revision":
				live.Data[journalKey] = "{"
			case "advance":
				raw, err := advanced.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				live.Data[journalKey] = string(raw)
			case "wrong-cluster":
				store.ClusterUID = "another-cluster"
			case "wrong-expected-uid":
				store.ExpectedUID = "another-journal"
			case "empty-snapshot":
				prior = Snapshot{}
			}
			resource := corev1.SchemeGroupVersion.WithResource("configmaps")
			if name == "deleted" {
				err = api.Tracker().Delete(resource, live.Namespace, live.Name)
			} else {
				err = api.Tracker().Update(resource, live, live.Namespace)
			}
			if err != nil {
				t.Fatal(err)
			}
			api.ClearActions()
			err := store.RecheckExecution(context.Background(), prior)
			if (err == nil) != wantOK {
				t.Fatalf("accepted=%v, want %v: %v", err == nil, wantOK, err)
			}
			if actions := api.Actions(); len(actions) != 1 || actions[0].GetVerb() != "get" {
				t.Fatalf("recheck must perform one fresh GET, got %v", actions)
			}
		})
	}
}

func TestStoreRecheckExecutionCancellationAndDeniedRead(t *testing.T) {
	store, before := checkpointFixture(t, transitionFixture(t))
	for _, name := range []string{"already-cancelled", "cancelled-during-read", "forbidden"} {
		t.Run(name, func(t *testing.T) {
			api := fake.NewClientset(before.object.DeepCopy())
			store.ConfigMaps = api.CoreV1().ConfigMaps(before.object.Namespace)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			api.PrependReactor("get", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
				if name == "forbidden" {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, store.Name, errors.New("denied"))
				}
				cancel()
				return true, before.object.DeepCopy(), nil
			})
			if name == "already-cancelled" {
				cancel()
			}
			err := store.RecheckExecution(ctx, before)
			if name == "forbidden" {
				if !apierrors.IsForbidden(err) {
					t.Fatalf("expected live access denial: %v", err)
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation: %v", err)
			}
			if name == "already-cancelled" && len(api.Actions()) != 0 {
				t.Fatal("cancelled recheck started API work")
			}
		})
	}
}
