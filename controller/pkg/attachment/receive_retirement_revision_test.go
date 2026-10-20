package attachment

import (
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Metadata classification only; live admission and immutable-history checks
// are covered by the real-API joint retirement fixture.
func TestRetirementSourceRevision(t *testing.T) {
	base := metav1.ObjectMeta{Name: "inventory", Namespace: "test", UID: "original", ResourceVersion: "10"}
	for _, tc := range []struct {
		name         string
		change       func(*metav1.ObjectMeta, *metav1.ObjectMeta)
		ok, conflict bool
	}{
		{"unchanged", func(_, _ *metav1.ObjectMeta) {}, true, false},
		{"revision", func(_, b *metav1.ObjectMeta) { b.ResourceVersion = "11" }, false, true},
		{"replacement", func(_, b *metav1.ObjectMeta) { b.UID = "replacement"; b.ResourceVersion = "11" }, false, false},
		{"missing-original-uid", func(a, _ *metav1.ObjectMeta) { a.UID = "" }, false, false},
		{"missing-current-uid", func(_, b *metav1.ObjectMeta) { b.UID = "" }, false, false},
		{"renamed", func(_, b *metav1.ObjectMeta) { b.Name = "other" }, false, false},
		{"other-namespace", func(_, b *metav1.ObjectMeta) { b.Namespace = "other" }, false, false},
		{"missing-original-revision", func(a, _ *metav1.ObjectMeta) { a.ResourceVersion = "" }, false, false},
		{"missing-current-revision", func(_, b *metav1.ObjectMeta) { b.ResourceVersion = "" }, false, false},
		{"already-deleting", func(a, _ *metav1.ObjectMeta) { at := metav1.Now(); a.DeletionTimestamp = &at }, false, false},
		{"now-deleting", func(_, b *metav1.ObjectMeta) { at := metav1.Now(); b.DeletionTimestamp = &at; b.ResourceVersion = "11" }, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := base, base
			tc.change(&a, &b)
			err := retirementSourceRevision(a, b, "configmaps")
			if (err == nil) != tc.ok || apierrors.IsConflict(err) != tc.conflict {
				t.Fatalf("wrong classification: %v", err)
			}
		})
	}
}
