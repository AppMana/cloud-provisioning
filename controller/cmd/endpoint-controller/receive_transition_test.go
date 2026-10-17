package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"slices"
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
	// Verify publication ordering at the real API write boundary: no journal
	// may be created before discovery contains its immutable Machine identity.
	r.receiveJournal.ConfigMaps = inventoryCheckedCreates{ConfigMapInterface: store.ConfigMaps, check: func(ctx context.Context, object *corev1.ConfigMap) error {
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
		return nil
	}}
	now := time.Now()
	if err := r.ensureAdoptionConfig(ctx, machine); err != nil {
		t.Fatalf("normal adoption reconciliation did not commit a prepared plan: %v", err)
	}
	snapshot, err := store.Load(ctx)
	if err != nil {
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
	verifyReceiveInventory(t, r, string(machine.GetUID()))
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
}
