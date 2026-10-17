package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type recoveryNodeReadFailure struct {
	client.Reader
	reads int
}

func (r *recoveryNodeReadFailure) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Node); ok {
		r.reads++
		return fmt.Errorf("injected CNI node read failure")
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// Reuse the real-API publication fixture and exercise the production periodic
// path with a failed CNI read. Direct object writes below only seed test history.
func verifyReceiveCNIFailureRecovery(t *testing.T, r *meshReconciler, machine *unstructured.Unstructured, store handover.Store, snapshot handover.Snapshot) {
	t.Helper()
	ctx := context.Background()
	original, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	restore := func(object *corev1.ConfigMap) {
		current, e := store.ConfigMaps.Get(ctx, object.Name, metav1.GetOptions{})
		if e != nil {
			t.Error(e)
			return
		}
		current.Data = object.Data
		if _, e = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); e != nil {
			t.Error(e)
		}
	}
	defer restore(original.DeepCopy())
	v, err := snapshot.Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := r.receivePreparation.Journal.policy()
	if err != nil {
		t.Fatal(err)
	}
	expired, err := handover.NewTransition(v.Round.Spec, time.Now().Add(-time.Hour), policy)
	if err != nil {
		t.Fatal(err)
	}
	seedStore := store
	seedStore.Name += "-expired-fixture"
	if _, err = seedStore.CreateWithPlan(ctx, expired, snapshot.Plan()); err != nil {
		t.Fatal(err)
	}
	defer seedStore.ConfigMaps.Delete(ctx, seedStore.Name, metav1.DeleteOptions{})
	seed, err := seedStore.ConfigMaps.Get(ctx, seedStore.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	original.Data = seed.Data
	if _, err = store.ConfigMaps.Update(ctx, original, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldReader, oldMode := r.reader, r.receivePreparation.Journal.ExpiryRecovery
	failure := &recoveryNodeReadFailure{Reader: oldReader}
	r.reader = failure
	defer func() { r.reader = oldReader; r.receivePreparation.Journal.ExpiryRecovery = oldMode }()
	r.receivePreparation.Journal.ExpiryRecovery = ""
	if err = r.refreshAdoptionConfigs(ctx); err == nil || failure.reads == 0 {
		t.Fatal("CNI failure not exercised or surfaced", err)
	}
	unchanged, err := store.Load(ctx)
	if err != nil || unchanged.Metadata().ResourceVersion != before.Metadata().ResourceVersion {
		t.Fatal("manual recovery mutated journal", err)
	}
	r.receivePreparation.Journal.ExpiryRecovery = "rollback"
	if err = r.refreshAdoptionConfigs(ctx); err == nil {
		t.Fatal("recovery hid the CNI failure")
	}
	after, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := after.Transition().View()
	if err != nil || got.Round.Phase != handover.Abort || !got.RollbackRequested || got.Complete || len(got.Observed) != 0 {
		t.Fatal("expired published preparation did not request abort", err)
	}
	if !bytes.Equal(after.Plan(), before.Plan()) || after.Metadata().UID != before.Metadata().UID || after.Transition().Extends(before.Transition()) != nil {
		t.Fatal("recovery changed plan, identity or history")
	}
	mesh := &corev1.Secret{}
	if err = r.reader.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
		t.Fatal(err)
	}
	inventory, object, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		t.Fatal(err)
	}
	defer restore(object.DeepCopy())
	for _, publication := range []tunnel.ReceiveJournalPublication{{JournalUID: "replaced-journal"}, {Plan: snapshot.Plan()}} {
		inventory.Publications[string(machine.GetUID())] = publication
		raw, e := json.Marshal(inventory)
		if e != nil {
			t.Fatal(e)
		}
		object.Data["inventory.json"] = string(raw)
		object, err = store.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		err = r.recoverPublishedReceiveTransition(ctx, machine, got.Round.ExpiresAt)
		if publication.JournalUID != "" && err == nil {
			t.Fatal("accepted wrong published UID")
		}
		if publication.JournalUID == "" && err != nil {
			t.Fatal("pending publication failed", err)
		}
		unchanged, err = store.Load(ctx)
		if err != nil || unchanged.Metadata().ResourceVersion != after.Metadata().ResourceVersion {
			t.Fatal("unpublished or replaced journal mutated", err)
		}
	}
}
