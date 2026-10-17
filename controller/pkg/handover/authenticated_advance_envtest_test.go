package handover

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

type receiptRaceConfigMaps struct{ typedcore.ConfigMapInterface }

func (r receiptRaceConfigMaps) Update(ctx context.Context, object *corev1.ConfigMap, options metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	current, err := r.ConfigMapInterface.Get(ctx, object.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	current.Annotations = map[string]string{"concurrent-writer": current.ResourceVersion}
	if _, err := r.ConfigMapInterface.Update(ctx, current, options); err != nil {
		return nil, err
	}
	return r.ConfigMapInterface.Update(ctx, object, options)
}

// Reuse the real API and its two independently pod-bound credentials. This
// exercises token authentication, round admission and storage as one operation.
func verifyAuthenticatedAdvancement(t *testing.T, verifier PodReceiptAuthenticator, nodes []*corev1.Node, bearers []string) {
	t.Helper()
	ctx := context.Background()
	plan := []byte(`{"transport":"test-fixture"}`)
	spec := fixture(t).Spec
	spec.PlanHash = fmt.Sprintf("%x", sha256.Sum256(plan))
	for i := range spec.Members {
		spec.Members[i].NodeUID = string(nodes[i].UID)
	}
	now := time.Now()
	policy := Policy{RoundLifetime: time.Minute, MaxReceiptAge: 30 * time.Second, DrainInterval: time.Second}
	transition, err := NewTransition(spec, now, policy)
	if err != nil {
		t.Fatal(err)
	}
	store := Store{ConfigMaps: verifier.API.CoreV1().ConfigMaps(verifier.Namespace), Name: "authenticated-transition", ClusterUID: spec.ClusterUID}
	snapshot, err := store.CreateWithPlan(ctx, transition, plan)
	if err != nil {
		t.Fatal(err)
	}
	view, err := snapshot.Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := view.Round.Digest()
	if err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{}
	boots := map[string]string{}
	policies := map[string]PodReceiptAuthenticator{}
	for i, n := range nodes {
		tokens[string(n.UID)] = bearers[i]
		boots[string(n.UID)] = n.Status.NodeInfo.BootID
		policies[string(n.UID)] = verifier
	}
	submissions := []PodReceipt{}
	for _, m := range view.Round.Members {
		submissions = append(submissions, PodReceipt{BearerToken: tokens[m.NodeUID], BootID: boots[m.NodeUID], Receipt: Receipt{AgentContainerID: "containerd://running-agent", RoundHash: digest, NodeUID: m.NodeUID, FromKey: m.FromKey, ToKey: m.ToKey, DeviceID: m.DeviceID, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: time.Now()}})
	}
	baseline, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"partial", "duplicate", "cross-node-token", "wrong-boot", "wrong-round", "wrong-device", "stale-observation", "future-observation", "missing-policy", "old-container", "missing-container"} {
		t.Run("advance-"+fault, func(t *testing.T) {
			batch := append([]PodReceipt(nil), submissions...)
			configured := policies
			switch fault {
			case "partial":
				batch = batch[:1]
			case "duplicate":
				batch[1] = batch[0]
			case "cross-node-token":
				batch[0].BearerToken = batch[1].BearerToken
			case "wrong-boot":
				batch[0].BootID = "previous-boot"
			case "wrong-round":
				batch[0].Receipt.RoundHash = strings.Repeat("0", 64)
			case "wrong-device":
				batch[0].Receipt.ToKey = batch[0].Receipt.FromKey
			case "stale-observation":
				batch[0].Receipt.ObservedAt = now.Add(-time.Minute)
			case "future-observation":
				batch[0].Receipt.ObservedAt = now.Add(time.Hour)
			case "old-container":
				batch[0].Receipt.AgentContainerID = "containerd://previous-agent"
			case "missing-container":
				batch[0].Receipt.AgentContainerID = ""
			case "missing-policy":
				configured = map[string]PodReceiptAuthenticator{}
			}
			if fault != "partial" {
				if _, err := store.ObserveWithPodReceipts(ctx, digest, batch, configured); err == nil {
					t.Fatal("invalid observation changed the journal")
				}
			}
			if _, err := store.AdvanceWithPodReceipts(ctx, digest, batch, configured); err == nil {
				t.Fatal("invalid batch advanced the journal")
			}
			current, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if current.ResourceVersion != baseline.ResourceVersion || !reflect.DeepEqual(current.Data, baseline.Data) {
				t.Fatal("rejected batch changed persisted state")
			}
		})
	}
	for i := range submissions {
		observed, err := store.ObserveWithPodReceipts(ctx, digest, submissions[i:i+1], policies)
		if err != nil {
			t.Fatal(err)
		}
		v, err := observed.Transition().View()
		if err != nil || !reflect.DeepEqual(v.Round, view.Round) || len(v.Observed) != i+1 {
			t.Fatal("partial progress advanced or lost its round", err)
		}
	}
	next, err := store.AdvanceWithPodReceipts(ctx, digest, submissions, policies)
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := next.Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Round.Phase != Switch || advanced.Round.Nonce == view.Round.Nonce || len(advanced.Observed) != 0 {
		t.Fatal("verified batch did not commit exactly the next round")
	}
	persisted, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(persisted.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, bearer := range bearers {
		if strings.Contains(string(raw), bearer) {
			t.Fatal("bearer credential persisted in journal")
		}
	}
	encoded, err := json.Marshal(submissions)
	if err != nil {
		t.Fatal(err)
	}
	for _, bearer := range bearers {
		if strings.Contains(string(encoded), bearer) {
			t.Fatal("submission serialization exposed credential")
		}
	}
	restarted := store
	if _, err := restarted.AdvanceWithPodReceipts(ctx, digest, submissions, policies); err == nil {
		t.Fatal("replayed batch advanced a successor round")
	}
	current, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if current.ResourceVersion != persisted.ResourceVersion {
		t.Fatal("replay changed committed state")
	}
	racing := store
	racing.Name = "racing-authenticated-transition"
	if _, err := racing.CreateWithPlan(ctx, transition, plan); err != nil {
		t.Fatal(err)
	}
	racing.ConfigMaps = receiptRaceConfigMaps{store.ConfigMaps}
	if _, err := racing.ObserveWithPodReceipts(ctx, digest, submissions[:1], policies); !apierrors.IsConflict(err) {
		t.Fatal("observation ignored concurrent writer", err)
	}
	if _, err := racing.AdvanceWithPodReceipts(ctx, digest, submissions, policies); !apierrors.IsConflict(err) {
		t.Fatal("receipt advancement retried or ignored a concurrent writer", err)
	}
	raceResult, err := racing.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raceView, err := raceResult.Transition().View()
	if err != nil || raceView.Round.Nonce != view.Round.Nonce || raceView.Round.Phase != Prepare {
		t.Fatal("failed commit published a successor", err)
	}
	// Once switching may have started, rollback must authenticate fresh reverse
	// selection, drain and retirement rounds before reporting the old generation.
	rollback, err := next.Transition().Rollback(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	next, err = store.Commit(ctx, next, rollback)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []Phase{Switch, Drain, Retire} {
		v, err := next.Transition().View()
		if err != nil || v.Round.Phase != phase || !v.RollbackRequested || v.Round.ToGeneration != spec.FromGeneration {
			t.Fatal("rollback lost its phase or original destination", err)
		}
		hash, err := v.Round.Digest()
		if err != nil {
			t.Fatal(err)
		}
		batch := func() []PodReceipt {
			result := []PodReceipt{}
			for _, member := range v.Round.Members {
				result = append(result, PodReceipt{BearerToken: tokens[member.NodeUID], BootID: boots[member.NodeUID], Receipt: Receipt{AgentContainerID: "containerd://running-agent", RoundHash: hash, NodeUID: member.NodeUID, DeviceID: member.DeviceID, FromKey: member.FromKey, ToKey: member.ToKey, FromPeerKey: member.FromPeerKey, ToPeerKey: member.ToPeerKey, ObservedAt: time.Now()}})
			}
			return result
		}
		if phase == Drain {
			if _, err := store.AdvanceWithPodReceipts(ctx, hash, batch(), policies); err == nil {
				t.Fatal("rollback retired before drain deadline")
			}
			time.Sleep(time.Until(v.DrainUntil))
		}
		next, err = store.AdvanceWithPodReceipts(ctx, hash, batch(), policies)
		if err != nil {
			t.Fatal(err)
		}
	}
	completed, err := next.Transition().View()
	if err != nil || !completed.Complete || completed.ServingGeneration != spec.FromGeneration {
		t.Fatal("verified rollback did not complete at the original generation", err)
	}
	// An expired round cannot be renewed as a side effect of receipt submission.
	expiredStore := store
	expiredStore.Name = "expired-authenticated-transition"
	expiredPolicy := policy
	expiredPolicy.RoundLifetime = time.Nanosecond
	expiredPolicy.MaxReceiptAge = time.Nanosecond
	expired, err := NewTransition(spec, time.Now().Add(-time.Second), expiredPolicy)
	if err != nil {
		t.Fatal(err)
	}
	expiredSnapshot, err := expiredStore.CreateWithPlan(ctx, expired, plan)
	if err != nil {
		t.Fatal(err)
	}
	expiredView, err := expiredSnapshot.Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	expiredHash, err := expiredView.Round.Digest()
	if err != nil {
		t.Fatal(err)
	}
	for i := range submissions {
		submissions[i].Receipt.RoundHash = expiredHash
		submissions[i].Receipt.ObservedAt = expiredView.Round.IssuedAt
	}
	before, err := expiredStore.ConfigMaps.Get(ctx, expiredStore.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expiredStore.AdvanceWithPodReceipts(ctx, expiredHash, submissions, policies); err == nil {
		t.Fatal("expired round advanced")
	}
	after, err := expiredStore.ConfigMaps.Get(ctx, expiredStore.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if before.ResourceVersion != after.ResourceVersion {
		t.Fatal("expired round was changed or renewed")
	}
}
