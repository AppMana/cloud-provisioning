package tunnel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func nextReceiverGraph(t *testing.T, previous ReceiveTransitionPlan, view handover.View, generation string) ReceiveTransitionPlan {
	t.Helper()
	raw, _ := json.Marshal(previous)
	var next ReceiveTransitionPlan
	if err := json.Unmarshal(raw, &next); err != nil {
		t.Fatal(err)
	}
	var serving ReceiveTransitionPlan
	if view.ServingGeneration == "receivers:"+view.Round.PlanHash {
		serving = previous
		serving.Version = 1
		serving.PreviousReceiver = nil
	} else {
		serving = *previous.PreviousReceiver.Plan
	}
	// Copy the flat projection so mutations in negative cases cannot alter input.
	servingRaw, _ := json.Marshal(serving)
	var flat ReceiveTransitionPlan
	if err := json.Unmarshal(servingRaw, &flat); err != nil {
		t.Fatal(err)
	}
	next.Version, next.ReceiverGeneration = 2, generation
	next.PreviousReceiver = &ReceivePreviousReceiver{Generation: view.ServingGeneration, Plan: &flat}
	next.SiteBase = &flat.SiteDesired
	// A later allocation replaces the earlier added range while the previous
	// receiver must retain its exact source union throughout the transition.
	var sources []string
	for _, raw := range next.SiteDesired.Peers[0].WGAllowedIPs {
		if raw != "198.19.1.0/24" {
			sources = append(sources, raw)
		}
	}
	added := "198.19.1.0/24"
	if generation[0] == 'b' {
		added = "198.19.2.0/24"
	}
	next.SiteDesired.Peers[0].WGAllowedIPs = append(sources, added)

	scope, err := next.ReceiveScope()
	if err != nil {
		t.Fatal(err)
	}
	var base PeerListDoc
	if err := json.Unmarshal(next.Base, &base); err != nil {
		t.Fatal(err)
	}
	paths, err := PlanReceiveHandover(scope, base, next.Desired, next.Native)
	if err != nil {
		t.Fatal(err)
	}
	for i, path := range paths.Receivers {
		binding := next.Bindings[path.PublicKey]
		binding.Interface = path.Interface
		seed := byte(128 + i*8)
		if generation[0] == 'b' {
			seed += 32
		}
		binding.PublicKey = receiverTestKey(seed)
		binding.ListenPort += 100
		next.Bindings[path.PublicKey] = binding
		next.Ports[path.PublicKey] = binding.ListenPort
		site := next.Sites[path.PublicKey]
		site.Client, err = PlanSiteReceiveClient(scope, path.PublicKey, next.RootPublicKey, binding, next.PreviousSitePeers(), next.SiteDesired)
		if err != nil {
			t.Fatal(err)
		}
		next.Sites[path.PublicKey] = site
	}
	return next
}

// Storage is simulated with explicit UIDs. These tests bind graph contents and
// round keys, not API compare-and-swap, native switching or packet continuity.
func TestSuccessorReceiverGraphFollowsArchivedServingGeneration(t *testing.T) {
	ctx := context.Background()
	for _, fault := range []string{"forward", "abort", "rollback", "forged-permissions"} {
		t.Run(fault, func(t *testing.T) {
			previous, tr := journalRouteFixture(t, routeSetFixture(t)[0])
			for {
				view, err := tr.View()
				if err != nil {
					t.Fatal(err)
				}
				if view.Complete {
					break
				}
				at := view.Round.IssuedAt.Add(time.Second)
				if view.Round.Phase == handover.Drain {
					at = view.DrainUntil
				}
				tr = advanceJournalRoute(t, tr, at)
			}
			original := journalRouteSnapshot(t, previous, tr)
			history, _ := tr.Bytes()
			cm := &corev1.ConfigMap{ObjectMeta: original.Metadata(), Data: map[string]string{"transition.json": string(history), "plan.json": string(original.Plan())}}
			api := fake.NewClientset(cm)
			serial := 0
			api.PrependReactor("create", "configmaps", func(a ktesting.Action) (bool, runtime.Object, error) {
				serial++
				o := a.(ktesting.CreateAction).GetObject().(*corev1.ConfigMap)
				o.UID = types.UID(fmt.Sprintf("archive-%d", serial))
				o.ResourceVersion = "1"
				return false, nil, nil
			})
			store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps(previous.Namespace), Name: cm.Name, ClusterUID: "cluster"}
			current, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, generation := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
				view, err := current.Transition().View()
				if err != nil {
					t.Fatal(err)
				}
				next := nextReceiverGraph(t, previous, view, generation)
				if fault == "forged-permissions" {
					next.PreviousReceiver.Plan.SiteDesired.Peers[0].WGAllowedIPs = append(next.PreviousReceiver.Plan.SiteDesired.Peers[0].WGAllowedIPs, "198.19.254.0/24")
					for _, p := range []*ReceiveTransitionPlan{next.PreviousReceiver.Plan, &next} {
						scope, err := p.ReceiveScope()
						if err != nil {
							t.Fatal(err)
						}
						for key, site := range p.Sites {
							site.Client, err = PlanSiteReceiveClient(scope, key, p.RootPublicKey, p.Bindings[key], p.PreviousSitePeers(), p.SiteDesired)
							if err != nil {
								t.Fatal(err)
							}
							p.Sites[key] = site
						}
					}
				}
				raw, err := json.Marshal(next)
				if err != nil {
					t.Fatal(err)
				}
				spec, err := ReceiveTransitionSpec(next, "cluster", fmt.Sprintf("%x", sha256.Sum256(raw)))
				if err != nil {
					t.Fatal(err)
				}
				for _, member := range spec.Members {
					if member.NodeUID == next.CloudNodeUID && member.FromKey == next.RootPublicKey {
						t.Fatal("successor started from original root key")
					}
				}
				current, err = store.ReplaceCompleted(ctx, current, spec, raw, view.Round.IssuedAt.Add(10*time.Second), handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				err = ValidateReceiveTransportHistory(ctx, store, current)
				if fault == "forged-permissions" {
					if err == nil {
						t.Fatal("forged serving permissions admitted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.ArchiveNames(ctx, current); err != nil {
					t.Fatal(err)
				}
				rolled := false
				for {
					view, err := current.Transition().View()
					if err != nil {
						t.Fatal(err)
					}
					intent, err := PlanReceiveJournalRoutes([]handover.Snapshot{current}, "cluster")
					if err != nil {
						t.Fatal(err)
					}
					selected := next
					if view.Round.Phase == handover.Prepare || view.RollbackRequested {
						selected = *next.PreviousReceiver.Plan
					}
					want, err := PlanReceiveRoutes(selected)
					if err != nil {
						t.Fatal(err)
					}
					if len(intent.Legacy) != 0 || !reflect.DeepEqual(intent.Receivers, want) {
						t.Fatal("successor selected the wrong receiver generation")
					}
					if view.Complete {
						sitePeers, err := next.CompletedSitePeers(view)
						if err != nil || !reflect.DeepEqual(sitePeers, selected.SiteDesired) {
							t.Fatal("terminal successor lost serving site permissions", err)
						}
						break
					}
					at := view.Round.IssuedAt.Add(time.Second)
					if view.Round.Phase == handover.Drain {
						at = view.DrainUntil
					}

					var advanced handover.Transition
					if !rolled && ((fault == "abort" && view.Round.Phase == handover.Prepare) || (fault == "rollback" && view.Round.Phase == handover.Switch)) {
						advanced, err = current.Transition().Rollback(at)
						if err != nil {
							t.Fatal(err)
						}
						rolled = true
					} else {
						advanced = advanceJournalRoute(t, current.Transition(), at)
					}

					current, err = store.Commit(ctx, current, advanced)
					if err != nil {
						t.Fatal(err)
					}
				}
				previous = next
			}
		})
	}
}

func TestPreviousReceiverRejectsIdentityConflicts(t *testing.T) {
	previous, tr := journalRouteFixture(t, routeSetFixture(t)[0])
	for {
		view, err := tr.View()
		if err != nil {
			t.Fatal(err)
		}
		if view.Complete {
			break
		}
		at := view.Round.IssuedAt.Add(time.Second)
		if view.Round.Phase == handover.Drain {
			at = view.DrainUntil
		}
		tr = advanceJournalRoute(t, tr, at)
	}
	view, _ := tr.View()
	for _, fault := range []string{"downgrade", "missing", "nested", "generation", "reused-generation", "node", "participant", "key", "port", "interface", "base", "site-base", "allocation"} {
		t.Run(fault, func(t *testing.T) {
			next := nextReceiverGraph(t, previous, view, strings.Repeat("a", 64))
			key := receiverTestKey(16)
			binding := next.Bindings[key]
			switch fault {
			case "downgrade":
				next.Version = 1
			case "missing":
				next.PreviousReceiver = nil
			case "nested":
				next.PreviousReceiver.Plan.PreviousReceiver = &ReceivePreviousReceiver{}
			case "generation":
				next.PreviousReceiver.Generation = "receivers:invalid"
			case "reused-generation":
				next.ReceiverGeneration = previous.ReceiverGeneration
			case "node":
				next.CloudNodeUID = "replacement"
			case "participant":
				site := next.Sites[key]
				site.NodeUID = "replacement"
				next.Sites[key] = site
			case "key":
				binding.PublicKey = previous.Bindings[key].PublicKey
				next.Bindings[key] = binding
			case "port":
				binding.ListenPort = previous.Bindings[key].ListenPort
				next.Bindings[key] = binding
				next.Ports[key] = binding.ListenPort
			case "interface":
				binding.Interface = previous.Bindings[key].Interface
				next.Bindings[key] = binding
			case "base":
				next.Base = []byte(`{"peers":[]}`)
			case "site-base":
				next.SiteBase = nil
			case "allocation":
				next.CloudAddresses = []string{"172.29.55.1/32"}
			}
			if _, err := PlanReceiveRoutes(next); err == nil {
				t.Fatal("unsafe previous graph accepted")
			}
		})
	}
}
