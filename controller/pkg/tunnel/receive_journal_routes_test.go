package tunnel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var journalRouteEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func journalRouteFixture(t *testing.T, p ReceiveTransitionPlan) (ReceiveTransitionPlan, handover.Transition) {
	t.Helper()
	p.MachineName = "worker-" + p.MachineUID
	p.AdoptionName = AdoptionSecretName(p.MachineName)
	p.AdoptionUID = p.MachineUID + "-adoption"
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ReceiveTransitionSpec(p, "cluster", fmt.Sprintf("%x", sha256.Sum256(raw)))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := handover.NewTransition(spec, journalRouteEpoch, handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return p, tr
}
func journalRouteSnapshot(t *testing.T, p ReceiveTransitionPlan, tr handover.Transition) handover.Snapshot {
	t.Helper()
	plan, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	history, err := tr.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	name := ReceiveTransitionName(p.MachineUID)
	object := corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.Namespace, UID: types.UID(p.MachineUID + "-journal"), ResourceVersion: "opaque"}, Data: map[string]string{"plan.json": string(plan), "transition.json": string(history)}}
	raw, err := json.Marshal(map[string]any{"version": 1, "object": object})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (handover.Store{Name: name, ClusterUID: "cluster"}).RestoreCheckpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
func advanceJournalRoute(t *testing.T, tr handover.Transition, at time.Time) handover.Transition {
	t.Helper()
	view, err := tr.View()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := view.Round.Digest()
	if err != nil {
		t.Fatal(err)
	}
	var proofs []handover.AuthenticatedReceipt
	for _, m := range view.Round.Members {
		proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: hash, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: at}})
	}
	next, err := tr.Advance(at, proofs)
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func TestJournalRoutesFollowCommittedDirectionAcrossRestart(t *testing.T) {
	p, initial := journalRouteFixture(t, routeSetFixture(t)[0])
	check := func(t *testing.T, tr handover.Transition, receivers bool) {
		t.Helper()
		snapshot := journalRouteSnapshot(t, p, tr)
		before, err := snapshot.Checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		intent, err := PlanReceiveJournalRoutes([]handover.Snapshot{snapshot}, "cluster")
		if err != nil {
			t.Fatal(err)
		}
		if receivers {
			want, err := PlanReceiveRoutes(p)
			if err != nil {
				t.Fatal(err)
			}
			if len(intent.Legacy) != 0 || !reflect.DeepEqual(want, intent.Receivers) {
				t.Fatal("did not select receivers")
			}
		} else {
			if len(intent.Legacy) != 1 || intent.Legacy[p.MachineUID].MachineUID != p.MachineUID {
				t.Fatal("lost explicit legacy intent")
			}
			for _, routes := range intent.Receivers {
				if len(routes) != 0 {
					t.Fatal("selected receivers during legacy phase")
				}
			}
		}
		after, err := snapshot.Checkpoint()
		if err != nil || string(before) != string(after) {
			t.Fatal("route derivation mutated journal")
		}
	}
	check(t, initial, false)
	switched := advanceJournalRoute(t, initial, journalRouteEpoch.Add(time.Second))
	check(t, switched, true)
	drained := advanceJournalRoute(t, switched, journalRouteEpoch.Add(2*time.Second))
	check(t, drained, true)
	retired := advanceJournalRoute(t, drained, journalRouteEpoch.Add(7*time.Second))
	check(t, retired, true)
	complete := advanceJournalRoute(t, retired, journalRouteEpoch.Add(8*time.Second))
	check(t, complete, true)
	for i, start := range []handover.Transition{initial, switched, drained} {
		t.Run(fmt.Sprintf("rollback-%d", i), func(t *testing.T) {
			at := journalRouteEpoch.Add(time.Duration(i+1) * time.Second)
			tr, err := start.Rollback(at)
			if err != nil {
				t.Fatal(err)
			}
			for {
				check(t, tr, false)
				view, err := tr.View()
				if err != nil {
					t.Fatal(err)
				}
				if view.Complete {
					break
				}
				at = at.Add(time.Second)
				if view.Round.Phase == handover.Drain {
					at = view.DrainUntil
				}
				tr = advanceJournalRoute(t, tr, at)
			}
		})
	}
	for _, tc := range []struct {
		tr        handover.Transition
		receivers bool
	}{{initial, false}, {switched, true}} {
		view, err := tc.tr.View()
		if err != nil {
			t.Fatal(err)
		}
		renewed, err := tc.tr.Renew(view.Round.ExpiresAt)
		if err != nil {
			t.Fatal(err)
		}
		check(t, renewed, tc.receivers)
	}
}
func TestJournalRoutesPreserveOtherWorkersDuringRollback(t *testing.T) {
	plans := routeSetFixture(t)
	first, a := journalRouteFixture(t, plans[0])
	second, b := journalRouteFixture(t, plans[1])
	a = advanceJournalRoute(t, a, journalRouteEpoch.Add(time.Second))
	check := func(active ReceiveTransitionPlan, legacy ReceiveTransitionPlan, a, b handover.Transition) {
		t.Helper()
		got, err := PlanReceiveJournalRoutes([]handover.Snapshot{journalRouteSnapshot(t, first, a), journalRouteSnapshot(t, second, b)}, "cluster")
		if err != nil {
			t.Fatal(err)
		}
		want, err := PlanReceiveRoutes(active)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Receivers["site"], want["site"]) || !reflect.DeepEqual(got.Receivers["retained"], want["retained"]) || len(got.Legacy) != 1 || got.Legacy[legacy.MachineUID].MachineUID != legacy.MachineUID {
			t.Fatal("mixed phases lost independent worker intent")
		}
	}
	check(first, second, a, b)
	b = advanceJournalRoute(t, b, journalRouteEpoch.Add(time.Second))
	var err error
	a, err = a.Rollback(journalRouteEpoch.Add(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	check(second, first, a, b)
}
func TestJournalRoutesValidateInactivePlansBeforeFiltering(t *testing.T) {
	plans := routeSetFixture(t)
	first, a := journalRouteFixture(t, plans[0])
	a = advanceJournalRoute(t, a, journalRouteEpoch.Add(time.Second))
	second := plans[1]
	second.SiteDesired.Peers[0].WGAllowedIPs[0] = "172.29.0.1/32"
	second.SiteDesired.Peers[0].RouteHosts = []string{"172.29.0.1"}
	refreshRouteSetClients(t, &second)
	second, b := journalRouteFixture(t, second)
	snapshots := []handover.Snapshot{journalRouteSnapshot(t, first, a), journalRouteSnapshot(t, second, b)}
	if got, err := PlanReceiveJournalRoutes(snapshots, "cluster"); err == nil || got.Receivers != nil || got.Legacy != nil {
		t.Fatal("inactive conflicting plan was filtered out")
	}
	if _, err := PlanReceiveJournalRoutes(snapshots[:1], "foreign"); err == nil {
		t.Fatal("foreign cluster accepted")
	}
	if _, err := PlanReceiveJournalRoutes([]handover.Snapshot{snapshots[0], snapshots[0]}, "cluster"); err == nil {
		t.Fatal("duplicate journal accepted")
	}
	if _, err := PlanReceiveJournalRoutes(nil, "cluster"); err == nil {
		t.Fatal("empty journal set authorized withdrawal")
	}
}
