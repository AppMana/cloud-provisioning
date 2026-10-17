package handover

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExecutionContinuation(t *testing.T) {
	original := transitionFixture(t)
	snapshot := func(tr Transition, revision string) Snapshot {
		t.Helper()
		raw, err := tr.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		return Snapshot{transition: tr, object: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "journal", Namespace: "test", UID: "journal-uid", ResourceVersion: revision}, Data: map[string]string{journalKey: string(raw)}}}
	}
	before := snapshot(original, "1")
	at := epoch.Add(time.Second)
	observed, err := original.Observe(at, receipts(t, view(t, original).Round, at)[:1])
	if err != nil {
		t.Fatal(err)
	}
	after := snapshot(observed, "2")
	if err := after.ContinuesExecution(before); err != nil {
		t.Fatal(err)
	}
	if err := after.ContinuesExecution(after); err != nil {
		t.Fatal(err)
	}
	if err := before.ContinuesExecution(after); err == nil {
		t.Fatal("accepted regressed observations")
	}
	for _, fault := range []string{"uid", "namespace", "name", "label", "owner", "plan", "deleting", "empty-revision"} {
		t.Run(fault, func(t *testing.T) {
			changed := snapshot(observed, "3")
			switch fault {
			case "uid":
				changed.object.UID = "replacement"
			case "namespace":
				changed.object.Namespace = "elsewhere"
			case "name":
				changed.object.Name = "replacement"
			case "label":
				changed.object.Labels = map[string]string{"changed": "true"}
			case "owner":
				changed.object.OwnerReferences = []metav1.OwnerReference{{UID: "replacement"}}
			case "plan":
				changed.object.Data[planKey] = "changed"
			case "deleting":
				now := metav1.Now()
				changed.object.DeletionTimestamp = &now
			case "empty-revision":
				changed.object.ResourceVersion = ""
			}
			if err := changed.ContinuesExecution(before); err == nil {
				t.Fatal("accepted changed object")
			}
		})
	}
	advanced := step(t, original, at)
	renewed, err := original.Renew(view(t, original).Round.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	rolledBack, err := original.Rollback(at)
	if err != nil {
		t.Fatal(err)
	}
	fork := transitionFixture(t)
	for name, tr := range map[string]Transition{"advance": advanced, "renew": renewed, "rollback": rolledBack, "new-nonce": fork} {
		t.Run(name, func(t *testing.T) {
			if err := snapshot(tr, "4").ContinuesExecution(before); err == nil {
				t.Fatal("accepted changed authority")
			}
		})
	}
	// Changing already committed observations is a fork even if the round matches.
	rewritten, err := original.Observe(at.Add(time.Second), receipts(t, view(t, original).Round, at.Add(time.Second))[:1])
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot(rewritten, "5").ContinuesExecution(after); err == nil {
		t.Fatal("accepted observation rewrite")
	}
}
