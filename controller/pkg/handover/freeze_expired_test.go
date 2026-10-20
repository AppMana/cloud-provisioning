package handover

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Real API CAS and immutable-data checks, with synthetic phase observations.
// This does not establish drain authority, native cleanup or safe release.
func verifyExpiredWithdrawalFreeze(t *testing.T, base Store) {
	ctx := context.Background()
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("persisted-intent")))
	for _, phase := range []Phase{Prepare, Switch, Drain, Retire, Abort} {
		t.Run("expired-withdrawal/"+string(phase), func(t *testing.T) {
			store := base
			store.Name = "expired-withdrawal-" + string(phase)
			tr := transportFixture(t)
			before, err := store.Create(ctx, tr)
			if err != nil {
				t.Fatal(err)
			}
			if phase == Abort {
				var err error
				tr, err = tr.Rollback(epoch.Add(time.Second))
				if err != nil {
					t.Fatal(err)
				}
				before, err = store.Commit(ctx, before, tr)
				if err != nil {
					t.Fatal(err)
				}
			}
			for view(t, tr).Round.Phase != phase {
				v := view(t, tr)
				at := v.Round.IssuedAt.Add(time.Second)
				if at.Before(v.DrainUntil) {
					at = v.DrainUntil
				}
				tr = step(t, tr, at)
				before, err = store.Commit(ctx, before, tr)
				if err != nil {
					t.Fatal(err)
				}
			}
			expires := view(t, tr).Round.ExpiresAt
			if _, err := store.FreezeExpiredWithdrawal(ctx, before, digest, expires.Add(-time.Nanosecond)); err == nil {
				t.Fatal("froze executable round")
			}
			if _, err := store.FreezeExpiredWithdrawal(ctx, before, "invalid", expires); err == nil {
				t.Fatal("accepted invalid intent")
			}
			frozen, err := store.FreezeExpiredWithdrawal(ctx, before, digest, expires)
			if err != nil {
				t.Fatal(err)
			}
			if !frozen.RetirementFrozen() || frozen.WithdrawalIntentHash() != digest || view(t, frozen.Transition()).Complete || !reflect.DeepEqual(frozen.Transition(), before.Transition()) {
				t.Fatal("freeze changed transport history or lost authority")
			}
			if err := frozen.ContinuesExecution(before); err == nil {
				t.Fatal("in-flight phase survived fence")
			}
			if err := frozen.Extends(before); err != nil {
				t.Fatal(err)
			}
			if err := before.Extends(frozen); err == nil {
				t.Fatal("checkpoint regressed fence")
			}
			if _, err := store.FreezeExpiredWithdrawal(ctx, before, digest, expires); !apierrors.IsConflict(err) {
				t.Fatalf("stale fence did not conflict: %v", err)
			}
			if _, err := store.FreezeExpiredWithdrawal(ctx, frozen, fmt.Sprintf("%x", sha256.Sum256([]byte("other"))), expires); err == nil {
				t.Fatal("changed intent accepted")
			}
			current, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.FreezeExpiredWithdrawal(ctx, current, digest, expires); err != nil {
				t.Fatal(err)
			}
			object, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			delete(object.Data, withdrawalIntentKey)
			if _, err := store.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
				t.Fatalf("API allowed dropping intent: %v", err)
			}
			restoredRaw, err := frozen.Checkpoint()
			if err != nil {
				t.Fatal(err)
			}
			restored, err := store.RestoreCheckpoint(restoredRaw)
			if err != nil || !restored.RetirementFrozen() || restored.WithdrawalIntentHash() != digest {
				t.Fatalf("checkpoint lost fence: %v", err)
			}
		})
	}
	t.Run("expired-withdrawal/renewal-wins", func(t *testing.T) {
		store := base
		store.Name = "expired-withdrawal-renewal"
		tr := transportFixture(t)
		before, err := store.Create(ctx, tr)
		if err != nil {
			t.Fatal(err)
		}
		at := view(t, tr).Round.ExpiresAt
		renewed, err := tr.Renew(at)
		if err != nil {
			t.Fatal(err)
		}
		current, err := store.Commit(ctx, before, renewed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.FreezeExpiredWithdrawal(ctx, before, digest, at); !apierrors.IsConflict(err) {
			t.Fatalf("stale withdrawal beat renewal: %v", err)
		}
		if _, err := store.FreezeExpiredWithdrawal(ctx, current, digest, at); err == nil {
			t.Fatal("fresh round frozen")
		}
	})
}
