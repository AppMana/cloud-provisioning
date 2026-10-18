package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Called with the controller's fully validated preparation fixture. Use real
// API storage to verify the adapter commits and resumes the exact public plan.
func verifyReceiveTransitionJournal(t *testing.T, r *meshReconciler, machine *unstructured.Unstructured, native []netip.Prefix, ports []int) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	k, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := k.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: machine.GetNamespace()}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	if err := r.configureReceiveJournal(ctx, k); err != nil {
		t.Fatal(err)
	}
	r.receivePreparation.Journal = &receiveJournalPolicy{RoundLifetime: metav1.Duration{Duration: time.Minute}, MaxReceiptAge: metav1.Duration{Duration: 10 * time.Second}, DrainInterval: metav1.Duration{Duration: 5 * time.Second}}
	defer func() { r.receivePreparation.Journal = nil }()
	store := r.receiveJournal
	store.Name = tunnel.ReceiveTransitionName(string(machine.GetUID()))
	for _, missing := range []string{tunnel.ReceiveReadyKey, tunnel.SiteReceiveReadyKey(preparationKey(8))} {
		adoption := &corev1.Secret{}
		key := client.ObjectKey{Namespace: machine.GetNamespace(), Name: tunnel.AdoptionSecretName(machine.GetName())}
		if err := r.reader.Get(ctx, key, adoption); err != nil {
			t.Fatal(err)
		}
		ready := slices.Clone(adoption.Data[missing])
		if len(ready) == 0 {
			t.Fatal("fixture lacks readiness to remove")
		}
		delete(adoption.Data, missing)
		if err := r.Update(ctx, adoption); err != nil {
			t.Fatal(err)
		}
		if err := r.reconcileReceivePreparation(ctx, machine); err != nil {
			t.Fatal("pending preparation failed", err)
		}
		mesh := &corev1.Secret{}
		if err := r.reader.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
			t.Fatal(err)
		}
		if _, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID)); !apierrors.IsNotFound(err) {
			t.Fatal("pending preparation fenced ordinary agents", err)
		}
		if _, err := store.Load(ctx); !apierrors.IsNotFound(err) {
			t.Fatal("pending preparation created a journal", err)
		}
		if err := r.reader.Get(ctx, key, adoption); err != nil {
			t.Fatal(err)
		}
		adoption.Data[missing] = ready
		if err := r.Update(ctx, adoption); err != nil {
			t.Fatal(err)
		}
	}
	// Verify publication ordering at the real API write boundary: no journal
	// may be created before discovery contains its immutable Machine identity.
	failCreate, failPublish := true, true
	r.receiveJournal.ConfigMaps = inventoryCheckedCreates{ConfigMapInterface: inventoryRacingUpdate{ConfigMapInterface: store.ConfigMaps, before: func() error {
		if failPublish {
			return fmt.Errorf("injected publication interruption")
		}
		return nil
	}}, check: func(ctx context.Context, object *corev1.ConfigMap) error {
		if object.Name != store.Name {
			return nil
		}
		mesh := &corev1.Secret{}
		if err := r.reader.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
			return err
		}
		inventory, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
		if err != nil || !slices.Contains(inventory.MachineUIDs, string(machine.GetUID())) {
			return fmt.Errorf("journal creation preceded its reservation: %v", err)
		}
		if inventory.CheckJournal(string(machine.GetUID()), "unpublished") == nil {
			return fmt.Errorf("pending journal was executable")
		}
		if failCreate {
			return fmt.Errorf("injected creation interruption")
		}
		return nil
	}}
	now := time.Now()
	if err := r.ensureAdoptionConfig(ctx, machine); err == nil {
		t.Fatal("creation interruption did not reach caller")
	}
	if _, err := store.Load(ctx); !apierrors.IsNotFound(err) {
		t.Fatal("interrupted creation left a journal", err)
	}
	// Recovery must use the durable prepared plan even after readiness has
	// disappeared; pending inventory prevents agents refreshing those receipts.
	adoption := &corev1.Secret{}
	adoptionKey := client.ObjectKey{Namespace: machine.GetNamespace(), Name: tunnel.AdoptionSecretName(machine.GetName())}
	if err := r.reader.Get(ctx, adoptionKey, adoption); err != nil {
		t.Fatal(err)
	}
	ready := slices.Clone(adoption.Data[tunnel.ReceiveReadyKey])
	delete(adoption.Data, tunnel.ReceiveReadyKey)
	if err := r.Update(ctx, adoption); err != nil {
		t.Fatal(err)
	}
	failCreate = false
	crashStore := r.receiveJournal
	crashStore.Name = store.Name
	crashPolicy, err := r.receivePreparation.Journal.policy()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ensureReceiveTransition(ctx, machine, crashStore, native, ports, crashPolicy, now.Add(-2*time.Minute)); err == nil {
		t.Fatal("publication interruption did not reach caller")
	}
	unpublished, err := store.Load(ctx)
	if err != nil {
		t.Fatal("reserved plan did not recover journal creation", err)
	}
	meshBeforePublish := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: r.secretName}, meshBeforePublish); err != nil {
		t.Fatal(err)
	}
	beforePublish, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, r.secretNamespace, r.secretName, string(meshBeforePublish.UID))
	if err != nil || beforePublish.CheckJournal(string(machine.GetUID()), string(unpublished.Metadata().UID)) == nil {
		t.Fatal("unpublished journal admitted", err)
	}
	failPublish = false
	if err := r.ensureAdoptionConfig(ctx, machine); err != nil {
		t.Fatalf("normal adoption reconciliation did not commit a prepared plan: %v", err)
	}
	snapshot, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Metadata().UID != unpublished.Metadata().UID || !bytes.Equal(snapshot.Plan(), unpublished.Plan()) {
		t.Fatal("publication recovery replaced the journal")
	}
	oldView, _ := unpublished.Transition().View()
	newView, _ := snapshot.Transition().View()
	if !now.After(oldView.Round.ExpiresAt) || !newView.Round.ExpiresAt.After(now) || oldView.Round.Nonce == newView.Round.Nonce || snapshot.Transition().Extends(unpublished.Transition()) != nil {
		t.Fatal("expired unpublished round was not renewed without regressing history")
	}
	published, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, r.secretNamespace, r.secretName, string(meshBeforePublish.UID))
	if err != nil || published.CheckJournal(string(machine.GetUID()), string(snapshot.Metadata().UID)) != nil || len(published.Publications[string(machine.GetUID())].Plan) != 0 {
		t.Fatal("publication failed to pin UID and discard seed", err)
	}
	if err := r.reader.Get(ctx, adoptionKey, adoption); err != nil {
		t.Fatal(err)
	}
	adoption.Data[tunnel.ReceiveReadyKey] = ready
	if err := r.Update(ctx, adoption); err != nil {
		t.Fatal(err)
	}
	view, err := snapshot.Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Round.Phase != handover.Prepare || view.Complete || len(view.Round.Members) != 4 {
		t.Fatal("journal did not begin with both transport endpoints in prepare")
	}
	var plan tunnel.ReceiveTransitionPlan
	if err := json.Unmarshal(snapshot.Plan(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.MachineUID != string(machine.GetUID()) || len(plan.Sites) != 2 {
		t.Fatal("journal lost controller plan")
	}
	t.Run("replaced-mesh-cannot-reserve-old-journal", func(t *testing.T) {
		mesh := &corev1.Secret{}
		if err := r.reader.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
			t.Fatal(err)
		}
		mesh.UID = "replacement-mesh"
		reader := fake.NewClientBuilder().WithScheme(r.Scheme()).WithObjects(mesh, machine.DeepCopy()).Build()
		replaced := *r
		replaced.Client, replaced.reader = reader, reader
		if _, err := replaced.ensureReceiveTransition(ctx, machine, store, native, ports, handover.Policy{}, now); err == nil {
			t.Fatal("resumed a journal from the previous mesh")
		}
		if _, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID)); !apierrors.IsNotFound(err) {
			t.Fatal("old journal reserved in replacement mesh", err)
		}
	})
	legacy := plan.Sites[preparationKey(16)].Legacy
	if legacy == nil || legacy.PeerPublicKey != preparationKey(8) || legacy.Transit == nil || legacy.Transit.Via != "10.101.0.1" {
		t.Fatal("journal lost the old relay while the desired mesh selected another site")
	}
	if legacy.Tunnel == nil || !slices.Equal(legacy.Tunnel.Addresses, []string{"10.100.0.2/24"}) || len(legacy.Tunnel.Hosts) != 0 {
		t.Fatal("new site's journal invented old direct tunnel permission")
	}
	retained := plan.Sites[preparationKey(8)].Legacy
	if retained == nil || retained.PeerPublicKey != preparationKey(8) || retained.Transit != nil {
		t.Fatal("journal lost the retained site's direct legacy path")
	}
	if retained.Tunnel == nil || !slices.Equal(retained.Tunnel.Addresses, []string{"10.100.0.1/24"}) || len(retained.Tunnel.Hosts) != 0 {
		t.Fatal("direct site's journal lost its allocation or added a source exception")
	}
	forward := plan.Sites[preparationKey(16)].Forward
	if forward == nil || forward.PeerPublicKey != preparationKey(16) || forward.Transit != nil {
		t.Fatal("journal did not bind the new relay's direct desired path")
	}
	retainedForward := plan.Sites[preparationKey(8)].Forward
	if retainedForward == nil || retainedForward.PeerPublicKey != preparationKey(8) || retainedForward.Transit != nil {
		t.Fatal("journal did not preserve the external site's desired direct ownership")
	}
	verifyReceiveInventory(t, r, string(machine.GetUID()))
	verifyReceiveReceiptAdmission(t, r, snapshot, plan)
	if err := r.ensureAdoptionConfig(ctx, machine); err != nil {
		t.Fatalf("normal reconciliation could not resume its journal: %v", err)
	}
	resumed, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resumedView, err := resumed.Transition().View()
	if err != nil || resumedView.Round.Nonce != view.Round.Nonce || !bytes.Equal(snapshot.Plan(), resumed.Plan()) {
		t.Fatal("normal reconciliation replaced the committed plan or round")
	}
	// An expired observation round remains the same persisted round on restart.
	// No new policy or missing transient preparation input may silently renew it.
	restored, err := r.ensureReceiveTransition(ctx, machine, store, nil, nil, handover.Policy{}, now.Add(time.Hour))
	if err != nil || restored == nil {
		t.Fatalf("restart could not restore the committed plan: %v", err)
	}
	restoredView, err := restored.Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	if restoredView.Round.Nonce != view.Round.Nonce || !bytes.Equal(snapshot.Plan(), restored.Plan()) {
		t.Fatal("restart reconstructed a different round or plan")
	}
	replaced := machine.DeepCopy()
	replaced.SetUID("replacement-machine")
	if _, err := r.ensureReceiveTransition(ctx, replaced, store, native, ports, handover.Policy{}, now); err == nil {
		t.Fatal("replacement machine adopted another journal")
	}
	object, err := k.CoreV1().ConfigMaps(machine.GetNamespace()).Get(ctx, store.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	original := object.DeepCopy()
	t.Run("CNI-failure-recovery", func(t *testing.T) { verifyReceiveCNIFailureRecovery(t, r, machine, store, snapshot) })
	for _, direction := range []string{"forward", "abort", "rollback"} {
		t.Run(direction, func(t *testing.T) {
			current, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			rolled := false
			for {
				v, err := current.Transition().View()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := r.ensureReceiveTransition(ctx, machine, store, nil, nil, handover.Policy{}, now); err != nil {
					t.Fatalf("could not resume committed %s/%s: %v", direction, v.Round.Phase, err)
				}
				verifyExpiredReceiveRecovery(t, ctx, store, current)
				if v.Complete {
					break
				}
				at := v.Round.IssuedAt.Add(time.Second)
				var next handover.Transition
				if !rolled && ((direction == "abort" && v.Round.Phase == handover.Prepare) || (direction == "rollback" && v.Round.Phase == handover.Switch)) {
					next, err = current.Transition().Rollback(at)
					rolled = true
				} else {
					if v.Round.Phase == handover.Drain {
						at = v.DrainUntil.Add(time.Millisecond)
					}
					digest, digestErr := v.Round.Digest()
					if digestErr != nil {
						t.Fatal(digestErr)
					}
					proofs := make([]handover.AuthenticatedReceipt, 0, len(v.Round.Members))
					for _, m := range v.Round.Members {
						proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: digest, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: at}})
					}
					partial := make([]handover.AuthenticatedReceipt, 0, len(proofs))
					for _, proof := range proofs {
						if proof.AuthenticatedNodeUID != "retained-uid" {
							partial = append(partial, proof)
						}
					}
					if len(partial) == len(proofs) {
						t.Fatal("round omitted the retained sender")
					}
					if _, err := current.Transition().Advance(at, partial); err == nil {
						t.Fatalf("advanced %s/%s without retained sender proof", direction, v.Round.Phase)
					}
					next, err = current.Transition().Advance(at, proofs)
				}
				if err != nil {
					t.Fatal(err)
				}
				current, err = store.Commit(ctx, current, next)
				if err != nil {
					t.Fatal(err)
				}
			}
			verifySuccessorPreparationProposal(t, r, machine, native, store, current)
			// Each terminal outcome must reject newly published sources without
			// rewriting the journal. This exercises controller admission against
			// real journal storage, not native forwarding or successor activation.
			mesh := &corev1.Secret{}
			meshKey := client.ObjectKey{Namespace: r.secretNamespace, Name: r.secretName}
			if err := r.reader.Get(ctx, meshKey, mesh); err != nil {
				t.Fatal(err)
			}
			sourceKey := tunnel.PeerAllowedIPsPrefix + machine.GetName()
			originalSources := slices.Clone(mesh.Data[sourceKey])
			for _, changed := range []string{string(originalSources) + ",198.18.0.0/24", string(originalSources) + ",2001:db8:ffff::/64", "198.18.0.1/32"} {
				mesh.Data[sourceKey] = []byte(changed)
				if err := r.Update(ctx, mesh); err != nil {
					t.Fatal(err)
				}
				if _, err := r.ensureReceiveTransition(ctx, machine, store, nil, nil, handover.Policy{}, now); err == nil || !strings.Contains(err.Error(), "successor transition required") {
					t.Fatal("completed journal did not report allocation drift", err)
				}
				after, err := store.Load(ctx)
				if err != nil {
					t.Fatal(err)
				}
				beforeJournal, err := current.Transition().Bytes()
				if err != nil {
					t.Fatal(err)
				}
				afterJournal, err := after.Transition().Bytes()
				if err != nil || !bytes.Equal(beforeJournal, afterJournal) || !bytes.Equal(after.Plan(), current.Plan()) {
					t.Fatal("allocation rejection changed the serving journal", err)
				}
				if err := r.reader.Get(ctx, meshKey, mesh); err != nil {
					t.Fatal(err)
				}
			}
			mesh.Data[sourceKey] = originalSources
			if err := r.Update(ctx, mesh); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ensureReceiveTransition(ctx, machine, store, nil, nil, handover.Policy{}, now); err != nil {
				t.Fatal("unchanged serving sources did not reconcile", err)
			}
			object, err = k.CoreV1().ConfigMaps(machine.GetNamespace()).Get(ctx, store.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			object.Data = original.DeepCopy().Data
			object, err = k.CoreV1().ConfigMaps(machine.GetNamespace()).Update(ctx, object, metav1.UpdateOptions{})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, fault := range []string{"unsupported-field", "trailing-data", "missing-retained-site", "route-outside-source", "node-binding", "device-binding", "key-binding", "peer-binding", "from-generation", "to-generation", "transition-identity"} {
		t.Run(fault, func(t *testing.T) {
			var changed map[string]any
			if err := json.Unmarshal(snapshot.Plan(), &changed); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "unsupported-field":
				changed["futureRoutePolicy"] = "required"
			case "missing-retained-site":
				delete(changed["sites"].(map[string]any), preparationKey(8))
			case "route-outside-source":
				var invalid tunnel.ReceiveTransitionPlan
				if err := json.Unmarshal(snapshot.Plan(), &invalid); err != nil {
					t.Fatal(err)
				}
				for i := range invalid.Desired.Peers {
					if !invalid.Desired.Peers[i].Remote {
						invalid.Desired.Peers[i].RouteHosts = []string{"0.0.0.0/0"}
					}
				}
				changed["desired"] = invalid.Desired
			}
			planRaw, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "trailing-data" {
				planRaw = append(planRaw, []byte(" {}")...)
			}
			spec := view.Round.Spec
			spec.PlanHash = fmt.Sprintf("%x", sha256.Sum256(planRaw))
			spec.TransitionUID = string(machine.GetUID()) + ":" + spec.PlanHash
			spec.ToGeneration = "receivers:" + spec.PlanHash
			spec.Members = append([]handover.Member(nil), spec.Members...)
			switch fault {
			case "node-binding":
				originalNode := spec.Members[0].NodeUID
				for i := range spec.Members {
					if spec.Members[i].NodeUID == originalNode {
						spec.Members[i].NodeUID = "another-node"
					}
				}
			case "device-binding":
				spec.Members[0].DeviceID += "-other"
			case "key-binding", "peer-binding":
				key, err := wgtypes.GeneratePrivateKey()
				if err != nil {
					t.Fatal(err)
				}
				if fault == "key-binding" {
					spec.Members[0].FromKey = key.PublicKey().String()
				} else {
					spec.Members[0].FromPeerKey = key.PublicKey().String()
				}
			case "from-generation":
				spec.FromGeneration = "legacy:another-document"
			case "to-generation":
				spec.ToGeneration = "receivers:another-document"
			case "transition-identity":
				spec.TransitionUID = "another-transition"
			}
			transition, err := handover.NewTransition(spec, now, handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := transition.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			object.Data["plan.json"], object.Data["transition.json"] = string(planRaw), string(raw)
			object, err = k.CoreV1().ConfigMaps(machine.GetNamespace()).Update(ctx, object, metav1.UpdateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(ctx); err != nil {
				t.Fatalf("fixture must have a matching storage hash: %v", err)
			}
			if err := r.ensureAdoptionConfig(ctx, machine); err == nil {
				t.Fatal("normal reconciliation resumed an unsupported or unsafe plan")
			}
			current, err := k.CoreV1().ConfigMaps(machine.GetNamespace()).Get(ctx, store.Name, metav1.GetOptions{})
			if err != nil || current.ResourceVersion != object.ResourceVersion {
				t.Fatal("rejected recovery rewrote the committed journal", err)
			}
		})
	}
	object.Data = original.Data
	object.Data["plan.json"] = "{}"
	if _, err := k.CoreV1().ConfigMaps(machine.GetNamespace()).Update(ctx, object, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureAdoptionConfig(ctx, machine); err == nil {
		t.Fatal("normal reconciliation accepted a corrupted journal plan")
	}
	// A published journal may have authorized native work. Deletion must not
	// resurrect its initial round, even though preparation is still available.
	if err := store.ConfigMaps.Delete(ctx, store.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureAdoptionConfig(ctx, machine); err == nil {
		t.Fatal("deleted published journal was recreated")
	}
	if _, err := store.Load(ctx); !apierrors.IsNotFound(err) {
		t.Fatal("deleted journal reappeared", err)
	}
}

// Exercise recovery against real API resource versions for every forward and
// reverse phase, without modifying the journal used by the progression fixture.
func verifyExpiredReceiveRecovery(t *testing.T, ctx context.Context, source handover.Store, snapshot handover.Snapshot) {
	t.Helper()
	object, err := source.ConfigMaps.Get(ctx, source.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	object.ObjectMeta = metav1.ObjectMeta{GenerateName: "recovery-", Namespace: object.Namespace}
	object, err = source.ConfigMaps.Create(ctx, object, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	store := source
	store.Name = object.Name
	defer store.ConfigMaps.Delete(ctx, store.Name, metav1.DeleteOptions{})
	before, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v, err := snapshot.Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	for _, trial := range []struct {
		mode string
		at   time.Time
	}{
		{"", v.Round.ExpiresAt}, {"rollback", v.Round.ExpiresAt.Add(-time.Nanosecond)},
	} {
		if err := recoverExpiredReceiveRound(ctx, store, before, trial.mode, trial.at); err != nil {
			t.Fatal(err)
		}
		unchanged, err := store.Load(ctx)
		if err != nil || unchanged.Metadata().ResourceVersion != before.Metadata().ResourceVersion {
			t.Fatal("manual or unexpired journal mutated", err)
		}
	}
	if err := recoverExpiredReceiveRound(ctx, store, before, "rollback", v.Round.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	after, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v.Complete {
		if after.Metadata().ResourceVersion != before.Metadata().ResourceVersion {
			t.Fatal("completed journal mutated")
		}
		return
	}
	got, err := after.Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	expected := v.Round.Spec
	reverse := !v.RollbackRequested && (v.Round.Phase == handover.Prepare || v.Round.Phase == handover.Switch || v.Round.Phase == handover.Drain)
	if reverse {
		if v.Round.Phase == handover.Prepare {
			expected.Phase = handover.Abort
		} else {
			expected.Phase = handover.Switch
			expected.FromGeneration, expected.ToGeneration = expected.ToGeneration, expected.FromGeneration
			expected.Members = slices.Clone(expected.Members)
			for i := range expected.Members {
				m := &expected.Members[i]
				m.FromKey, m.ToKey = m.ToKey, m.FromKey
				m.FromPeerKey, m.ToPeerKey = m.ToPeerKey, m.FromPeerKey
			}
		}
	}
	if !reflect.DeepEqual(expected, got.Round.Spec) || got.Complete || len(got.Observed) != 0 || got.Round.Nonce == v.Round.Nonce || got.RollbackRequested != (v.RollbackRequested || reverse) {
		t.Fatalf("incorrect recovery from %s rollback=%v: %+v", v.Round.Phase, v.RollbackRequested, got)
	}
	if !bytes.Equal(before.Plan(), after.Plan()) || before.Metadata().UID != after.Metadata().UID || after.Transition().Extends(before.Transition()) != nil {
		t.Fatal("recovery replaced journal identity, plan or history")
	}
	if err := recoverExpiredReceiveRound(ctx, store, before, "rollback", v.Round.ExpiresAt); !apierrors.IsConflict(err) {
		t.Fatal("stale recovery did not conflict", err)
	}
	if err := recoverExpiredReceiveRound(ctx, store, after, "rollback", got.Round.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	renewed, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rv, err := renewed.Transition().View()
	if err != nil || !reflect.DeepEqual(rv.Round.Spec, got.Round.Spec) || rv.RollbackRequested != got.RollbackRequested || rv.Round.Nonce == got.Round.Nonce {
		t.Fatal("repeated recovery reversed direction or reused round", err)
	}
}
