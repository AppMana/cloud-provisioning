package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

type deniedReceiveArchive struct {
	typedcore.ConfigMapInterface
	name string
}

func (r deniedReceiveArchive) Get(ctx context.Context, name string, options metav1.GetOptions) (*corev1.ConfigMap, error) {
	if name == r.name {
		return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, name, fmt.Errorf("injected archive read denial"))
	}
	return r.ConfigMapInterface.Get(ctx, name, options)
}

// Real API journal storage with simulated archive authorization denial. No host
// checkpoint is created: first admission must verify the whole pinned ancestry.
// This does not exercise native forwarding, interruption budgets or offline boot.
func verifyFreshReceiveHistory(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, root wgtypes.Key, additional ...func(handover.Store) error) {
	t.Helper()
	ctx := context.Background()
	original, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		current, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
		if err != nil {
			t.Error(err)
			return
		}
		current.Data = original.DeepCopy().Data
		if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-20 * time.Second)
	spec, err := tunnel.ReceiveTransitionSpec(plan, store.ClusterUID, fmt.Sprintf("%x", sha256.Sum256(snapshot.Plan())))
	if err != nil {
		t.Fatal(err)
	}
	policy := handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second}
	initial, err := handover.NewTransition(spec, start, policy)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := initial.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	seed := original.DeepCopy()
	seed.Data["transition.json"] = string(raw)
	if _, err = store.ConfigMaps.Update(ctx, seed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, generation := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		at := start.Add(time.Duration(1+3*i) * time.Second)
		aborted, err := snapshot.Transition().Rollback(at)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err = store.Commit(ctx, snapshot, aborted)
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
		var proofs []handover.AuthenticatedReceipt
		for _, m := range view.Round.Members {
			proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: digest, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: at.Add(time.Second)}})
		}
		terminal, err := snapshot.Transition().Advance(at.Add(time.Second), proofs)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err = store.Commit(ctx, snapshot, terminal)
		if err != nil {
			t.Fatal(err)
		}
		var next tunnel.ReceiveTransitionPlan
		if err := json.Unmarshal(snapshot.Plan(), &next); err != nil {
			t.Fatal(err)
		}
		next.ReceiverGeneration = generation
		receiveScope, err := next.ReceiveScope()
		if err != nil {
			t.Fatal(err)
		}
		var base tunnel.PeerListDoc
		if err := json.Unmarshal(next.Base, &base); err != nil {
			t.Fatal(err)
		}
		paths, err := tunnel.PlanReceiveHandover(receiveScope, base, next.Desired, next.Native)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths.Receivers {
			binding := next.Bindings[path.PublicKey]
			binding.Interface = path.Interface
			binding.PublicKey = receiverKey(root, receiveScope, path.PublicKey).PublicKey().String()
			binding.ListenPort += 100
			next.Bindings[path.PublicKey] = binding
			next.Ports[path.PublicKey] = binding.ListenPort
			site := next.Sites[path.PublicKey]
			site.Client, err = tunnel.PlanSiteReceiveClient(receiveScope, path.PublicKey, next.RootPublicKey, binding, next.PreviousSitePeers(), next.SiteDesired)
			if err != nil {
				t.Fatal(err)
			}
			next.Sites[path.PublicKey] = site
		}
		nextRaw, err := json.Marshal(next)
		if err != nil {
			t.Fatal(err)
		}
		nextSpec, err := tunnel.ReceiveTransitionSpec(next, store.ClusterUID, fmt.Sprintf("%x", sha256.Sum256(nextRaw)))
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err = store.ReplaceCompleted(ctx, snapshot, nextSpec, nextRaw, at.Add(2*time.Second), policy)
		if err != nil {
			t.Fatal(err)
		}
	}

	admissions := append([]func(handover.Store) error{func(candidate handover.Store) error {
		_, err := loadReceiveJournal(ctx, api, candidate, scope, plan.CloudNodeName, root)
		return err
	}}, additional...)
	check := func(label string, candidate handover.Store, want bool) {
		t.Helper()
		for i, admit := range admissions {
			err := admit(candidate)
			if (err == nil) != want {
				t.Fatalf("%s admission %d: %v", label, i, err)
			}
		}
	}
	check("intact history", store, true)
	names, err := store.ArchiveNames(ctx, snapshot)
	if err != nil || len(names) != 2 {
		t.Fatal("fixture did not retain two generations", err)
	}
	oldest := names[len(names)-1]
	denied := store
	denied.ConfigMaps = deniedReceiveArchive{ConfigMapInterface: store.ConfigMaps, name: oldest}
	check("denied history", denied, false)
	check("restored access", store, true)
	archived, err := store.ConfigMaps.Get(ctx, oldest, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfigMaps.Delete(ctx, oldest, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	check("missing history", store, false)
	recreated := archived.DeepCopy()
	recreated.ObjectMeta = metav1.ObjectMeta{Name: oldest, Namespace: archived.Namespace, Annotations: archived.Annotations}
	recreated, err = store.ConfigMaps.Create(ctx, recreated, metav1.CreateOptions{})
	if err != nil || recreated.UID == archived.UID {
		t.Fatal("archive recreation fixture failed", err)
	}
	check("recreated history", store, false)
}
