package handover

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func checkpointFixture(t testing.TB, tr Transition) (Store, Snapshot) {
	t.Helper()
	store := Store{Name: "transition", ClusterUID: "cluster-uid"}
	raw, err := tr.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.snapshot(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: store.Name, Namespace: "mesh", UID: "journal-uid", ResourceVersion: "opaque"}, Data: map[string]string{journalKey: string(raw)}})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := store.RestoreCheckpoint(saved)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := recovered.Transition().Bytes()
	if err != nil || !bytes.Equal(restored, raw) {
		t.Fatalf("checkpoint changed history: %v", err)
	}
	return store, recovered
}
func TestCheckpointPreservesHistoryAcrossMissedRounds(t *testing.T) {
	initial := transitionFixture(t)
	switched := step(t, initial, epoch.Add(time.Second))
	drained := step(t, switched, epoch.Add(2*time.Second))
	retired := step(t, drained, epoch.Add(7*time.Second))
	complete := step(t, retired, epoch.Add(8*time.Second))
	rolled, err := switched.Rollback(epoch.Add(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := switched.Renew(view(t, switched).Round.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	fork := step(t, initial, epoch.Add(time.Second))
	reminted, err := NewTransition(view(t, initial).Round.Spec, epoch, initial.j.Policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		next, previous Transition
		valid          bool
	}{
		{"unchanged", switched, switched, true},
		{"missed-rounds", complete, initial, true},
		{"rollback", rolled, switched, true},
		{"renew", renewed, switched, true},
		{"regression", initial, switched, false},
		{"same-phase-fork", fork, switched, false},
		{"rollback-fork", rolled, drained, false},
		{"renewal-fork", renewed, drained, false},
		{"reminted", reminted, initial, false},
		{"terminal-regression", retired, complete, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, next := checkpointFixture(t, tc.next)
			_, previous := checkpointFixture(t, tc.previous)
			if err := next.Extends(previous); (err == nil) != tc.valid {
				t.Fatalf("extension valid=%v: %v", tc.valid, err)
			}
		})
	}
}
func TestCheckpointRejectsCorruptionAndReplacement(t *testing.T) {
	store, original := checkpointFixture(t, transitionFixture(t))
	encoded, err := original.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*checkpoint)
	}{
		{"version", func(c *checkpoint) { c.Version++ }},
		{"missing-object", func(c *checkpoint) { c.Object = nil }},
		{"name", func(c *checkpoint) { c.Object.Name = "other" }},
		{"uid", func(c *checkpoint) { c.Object.UID = "" }},
		{"resource-version", func(c *checkpoint) { c.Object.ResourceVersion = "" }},
		{"deleted", func(c *checkpoint) { now := metav1.Now(); c.Object.DeletionTimestamp = &now }},
		{"transition", func(c *checkpoint) { c.Object.Data[journalKey] = "{}" }},
		{"plan-hash", func(c *checkpoint) { c.Object.Data[planKey] = "changed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c checkpoint
			if err := json.Unmarshal(encoded, &c); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&c)
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.RestoreCheckpoint(raw); err == nil {
				t.Fatal("accepted invalid checkpoint")
			}
		})
	}
	for _, bad := range [][]byte{append(append([]byte{}, encoded...), []byte(" {}")...), bytes.Replace(encoded, []byte(`"version":1`), []byte(`"version":1,"unknown":true`), 1), encoded[:len(encoded)/2]} {
		if _, err := store.RestoreCheckpoint(bad); err == nil {
			t.Fatal("accepted malformed checkpoint")
		}
	}
	foreign := store
	foreign.ClusterUID = "another-cluster"
	if _, err := foreign.RestoreCheckpoint(encoded); err == nil {
		t.Fatal("accepted foreign cluster")
	}
	for _, fault := range []string{"uid", "namespace", "name", "plan", "metadata"} {
		t.Run(fault, func(t *testing.T) {
			next := original
			next.object = original.object.DeepCopy()
			switch fault {
			case "uid":
				next.object.UID = "replacement"
			case "namespace":
				next.object.Namespace = "another-namespace"
			case "name":
				next.object.Name = "another-name"
			case "plan":
				next.object.Data[planKey] = "changed"
			case "metadata":
				next.object.ResourceVersion = "rv-after"
				next.object.Labels = map[string]string{"new": "label"}
			}
			if err := next.Extends(original); (err == nil) != (fault == "metadata") {
				t.Fatalf("unexpected identity result: %v", err)
			}
		})
	}
}
