package main

import (
	"context"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Real API revisions exercise operation guards. This helper deliberately does
// not authenticate synthetic finalization or claim native address-reuse safety.
func verifyReleasedJournalExecutionGuard(t *testing.T, released handover.Store, guard func() error) {
	t.Helper()
	ctx := context.Background()
	if err := guard(); err != nil {
		t.Fatal(err)
	}
	original, err := released.ConfigMaps.Get(ctx, released.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		current, err := released.ConfigMaps.Get(ctx, released.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		current.Annotations = original.Annotations
		if _, err := released.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}()
	changed := original.DeepCopy()
	if changed.Annotations == nil {
		changed.Annotations = map[string]string{}
	}
	changed.Annotations["test-released-history-race"] = "changed"
	if _, err := released.ConfigMaps.Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := guard(); err == nil {
		t.Fatal("released history mutation escaped native execution guard")
	}
}
