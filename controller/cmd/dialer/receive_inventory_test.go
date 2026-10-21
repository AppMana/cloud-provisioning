package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

type receiveSetReader struct {
	typedcore.ConfigMapInterface
	before func(string) error
}

func (r receiveSetReader) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error) {
	if err := r.before(name); err != nil {
		return nil, err
	}
	return r.ConfigMapInterface.Get(ctx, name, opts)
}

// The callback is a simulated native verifier. These tests prove complete-set
// admission and durable fencing, not network continuity or native completion.
func verifyReceiveSetAdmission(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, mesh *corev1.Secret, first tunnel.ReceiveTransitionPlan, site, firstRoot wgtypes.Key) {
	t.Helper()
	ctx := context.Background()
	_, originalIndex, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _ := wgtypes.GeneratePrivateKey()
	other := first
	other.MachineName, other.MachineUID = "worker-two", "second-machine-uid"
	other.CloudNodeName, other.CloudNodeUID = "cloud-two", "second-cloud-uid"
	other.RootPublicKey = otherKey.PublicKey().String()
	cloudNode, err := api.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: other.CloudNodeName}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	boot, err := localReceiveBootID()
	if err != nil {
		t.Fatal(err)
	}
	cloudNode.Status.NodeInfo.BootID = boot
	cloudNode.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	cloudNode, err = api.CoreV1().Nodes().UpdateStatus(ctx, cloudNode, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other.CloudNodeUID = string(cloudNode.UID)

	adoption, err := api.CoreV1().Secrets(scope.Namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName(other.MachineName)}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other.AdoptionName, other.AdoptionUID = adoption.Name, string(adoption.UID)
	oldBinding := first.Bindings[site.PublicKey().String()]
	binding := oldBinding
	binding.PublicKey = receiverKey(otherKey, scope.MeshUID, site.PublicKey().String()).PublicKey().String()
	other.Bindings = map[string]tunnel.ReceiveBinding{site.PublicKey().String(): binding}
	other.SiteDesired = tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: other.RootPublicKey, Remote: true, Endpoint: "198.51.100.2:51820", WGAllowedIPs: []string{"172.29.0.2/32"}}}}
	client, err := tunnel.PlanSiteReceiveClient(scope.MeshUID, site.PublicKey().String(), other.RootPublicKey, binding, other.SiteDesired, other.SiteDesired)
	if err != nil {
		t.Fatal(err)
	}
	siteInfo := first.Sites[site.PublicKey().String()]
	siteInfo.Client = client
	other.Sites = map[string]tunnel.ReceiveTransitionSite{site.PublicKey().String(): siteInfo}
	if err := tunnel.ReserveReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, other.MachineUID, store.OwnerReferences); err != nil {
		t.Fatal(err)
	}
	otherStore := store
	otherStore.Name = tunnel.ReceiveTransitionName(other.MachineUID)
	defer func() {
		if err := store.ConfigMaps.Delete(ctx, otherStore.Name, metav1.DeleteOptions{}); err != nil {
			t.Error(err)
		}
		current, err := store.ConfigMaps.Get(ctx, originalIndex.Name, metav1.GetOptions{})
		if err != nil {
			t.Error(err)
			return
		}
		current.Data = originalIndex.Data
		if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Error(err)
		}
	}()
	raw, _ := json.Marshal(other)
	spec, err := tunnel.ReceiveTransitionSpec(other, store.ClusterUID, fmt.Sprintf("%x", sha256.Sum256(raw)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tr, err := handover.NewTransition(spec, now, handover.Policy{RoundLifetime: 45 * time.Second, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := otherStore.CreateWithPlan(ctx, tr, raw)
	if err != nil {
		t.Fatal(err)
	}
	tr = snapshot.Transition()
	view, _ := tr.View()
	hash, _ := view.Round.Digest()
	var proofs []handover.AuthenticatedReceipt
	for _, m := range view.Round.Members {
		proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: hash, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: now}})
	}
	next, err := tr.Advance(now, proofs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherStore.Commit(ctx, snapshot, next); err != nil {
		t.Fatal(err)
	}
	firstSnapshot, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	firstView, err := firstSnapshot.Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	// API setup can consume more than the difference between round lifetimes.
	// Assert the actual earliest committed expiry, not creation order.
	expectedDeadline := now.Add(45 * time.Second)
	if firstView.Round.ExpiresAt.Before(expectedDeadline) {
		expectedDeadline = firstView.Round.ExpiresAt
	}
	cfg := config{iface: "wgset", nodeName: "site", privateKeyFile: filepath.Join(t.TempDir(), "key")}
	calls := 0
	verify := func(ctx context.Context, plan tunnel.ReceiveTransitionPlan, view handover.View, uid string, routes tunnel.ReceiveJournalRoutes) error {
		calls++
		if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(expectedDeadline) {
			t.Fatalf("combined work deadline %s (present=%v), want earliest committed expiry %s", deadline, ok, expectedDeadline)
		}
		if plan.MachineUID != first.MachineUID || view.Round.Phase != handover.Prepare || uid != siteInfo.NodeUID {
			t.Fatal("wrong target passed to native callback")
		}
		sources, found := routes.Sources[uid]
		if !found || len(sources.Addresses) != 1 || sources.Addresses[0] != "10.90.0.2/24" || len(sources.Hosts) != 0 {
			t.Fatal("native callback lost the admitted site allocation or invented source exceptions")
		}
		selection, found := routes.Receivers[uid]["172.29.0.2/32"]
		if !found || selection.Interface != client.Interface || len(routes.Receivers[uid]) != 1 || len(routes.Legacy) != 1 || routes.Legacy[first.MachineUID].MachineUID != first.MachineUID {
			t.Fatal("combined intent lost the other worker or selected the preparing worker")
		}
		return nil
	}
	if got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", verify); err != nil || len(got) != 1 || calls != 1 {
		t.Fatalf("two-worker execution admission: %v", err)
	}
	t.Run("independent-live-rechecks", func(t *testing.T) {
		// Measure the final named read for each member, then hold those reads
		// until both arrive. This exercises full admission, including a nonlocal
		// owner, without treating a cached snapshot as live authority.
		counts := map[string]int{}
		var mu sync.Mutex
		counted := store
		counted.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
			mu.Lock()
			counts[name]++
			mu.Unlock()
			return nil
		}}
		if _, err := loadReceiveSet(ctx, api, counted, scope, first.CloudNodeName, firstRoot); err != nil {
			t.Fatal(err)
		}
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		arrived := 0
		ready := make(chan struct{})
		reads := map[string]int{}
		counted.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
			mu.Lock()
			reads[name]++
			final := (name == store.Name || name == otherStore.Name) && reads[name] == counts[name]
			if final {
				arrived++
				if arrived == 2 {
					close(ready)
				}
			}
			mu.Unlock()
			if final {
				select {
				case <-ready:
				case <-bounded.Done():
					return bounded.Err()
				}
			}
			return nil
		}}
		admitted, err := loadReceiveSet(bounded, api, counted, scope, first.CloudNodeName, firstRoot)
		if err != nil || arrived != 2 || len(admitted.Journals) != 2 || len(admitted.Local) != 1 {
			t.Fatalf("independent live rechecks did not admit the complete set: arrivals=%d error=%v", arrived, err)
		}
	})
	t.Run("target-journal-incarnation", func(t *testing.T) {
		pinned := store
		pinned.ExpectedUID = string(firstSnapshot.Metadata().UID)
		cloudSet, err := loadReceiveSet(ctx, api, pinned, scope, first.CloudNodeName, firstRoot)
		if err != nil || len(cloudSet.Journals) != 2 || len(cloudSet.Local) != 1 {
			t.Fatal("target UID pin lost nonlocal journal admission", err)
		}
		fresh := cfg
		fresh.privateKeyFile = filepath.Join(t.TempDir(), "key")
		before := calls
		if got, err := executeReceivePhase(ctx, api, pinned, scope, fresh, site, "container", verify); err != nil || len(got) != 1 || calls != before+1 {
			t.Fatal("target UID pin rejected a different reserved journal", err)
		}
		pinned.ExpectedUID = "replaced-target"
		before = calls
		if got, err := executeReceivePhase(ctx, api, pinned, scope, fresh, site, "container", verify); err == nil || len(got) != 0 || calls != before {
			t.Fatal("wrong target UID permitted native execution", err)
		}
	})
	lockPath := filepath.Join(filepath.Dir(cfg.privateKeyFile), cfg.iface+".reconcile.lock")
	for _, suffix := range []string{"inventory.json", store.Name + ".json", otherStore.Name + ".json"} {
		if info, err := os.Stat(lockPath + "." + suffix); err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("missing private complete-set checkpoint %s: %v", suffix, err)
		}
	}
	t.Run("live-boot-recovery-of-complete-set", func(t *testing.T) {
		paths := []string{lockPath + ".inventory.json", lockPath + "." + store.Name + ".json", lockPath + "." + otherStore.Name + ".json"}
		originals := map[string][]byte{}
		for _, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			originals[path] = raw
		}
		defer func() {
			for path, raw := range originals {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Error(err)
				}
			}
		}()
		previousBoot := func(path string) []byte {
			t.Helper()
			var saved map[string]json.RawMessage
			if err := json.Unmarshal(originals[path], &saved); err != nil {
				t.Fatal(err)
			}
			saved["bootID"] = json.RawMessage(`"previous-boot"`)
			raw, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			return raw
		}
		oldIndex := previousBoot(paths[0])
		previousBoot(paths[1])
		previousBoot(paths[2])
		before := calls
		if got, err := executeReceivePhase(ctx, nil, store, scope, cfg, site, "container", verify); err == nil || len(got) != 0 || calls != before {
			t.Fatal("boot recovery executed without API admission")
		}
		// Simulate a crash after updating one local journal but before the rest
		// of the set. A missing second journal must still fence all native work.
		if err := os.WriteFile(paths[1], originals[paths[1]], 0600); err != nil {
			t.Fatal(err)
		}
		broken := store
		broken.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
			if name == otherStore.Name {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
			}
			return nil
		}}
		if got, err := executeReceivePhase(ctx, api, broken, scope, cfg, site, "container", verify); err == nil || len(got) != 0 || calls != before {
			t.Fatal("incomplete boot recovery executed native work")
		}
		raw, err := os.ReadFile(paths[0])
		if err != nil || !bytes.Equal(raw, oldIndex) {
			t.Fatal("failed boot recovery advanced inventory", err)
		}
		got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", verify)
		if err != nil || len(got) != 1 || calls != before+1 {
			t.Fatal("live complete set could not resume interrupted boot admission", err)
		}
		boot, err := localReceiveBootID()
		if err != nil || got[0].BootID != boot {
			t.Fatal("fresh verification retained old receipt boot", err)
		}
		for _, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				BootID string `json:"bootID"`
			}
			if err := json.Unmarshal(raw, &saved); err != nil || saved.BootID != boot {
				t.Fatal("checkpoint set retained earlier boot", err)
			}
		}
	})
	t.Run("missing-other-journal", func(t *testing.T) {
		broken := store
		broken.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
			if name == otherStore.Name {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
			}
			return nil
		}}
		before := calls
		if got, err := executeReceivePhase(ctx, api, broken, scope, cfg, site, "container", verify); !apierrors.IsNotFound(err) || len(got) != 0 || calls != before {
			t.Fatal("missing other worker permitted native work")
		}
	})
	// Receipt writers may append while a different participant is checking the
	// same round. Exercise real API CAS writes both during admission and after
	// native verification; neither may starve that participant's receipt.
	for _, target := range []handover.Store{store, otherStore} {
		for _, during := range []string{"read", "native"} {
			t.Run("observation-during-"+during+"-"+target.Name, func(t *testing.T) {
				appendObservation := func() error {
					current, err := target.Load(ctx)
					if err != nil {
						return err
					}
					v, err := current.Transition().View()
					if err != nil {
						return err
					}
					hash, err := v.Round.Digest()
					if err != nil {
						return err
					}
					m := v.Round.Members[0]
					at := time.Now()
					proof := handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: hash, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: at}}
					next, err := current.Transition().Observe(at, []handover.AuthenticatedReceipt{proof})
					if err != nil {
						return err
					}
					_, err = target.Commit(ctx, current, next)
					return err
				}
				raced, native := store, verify
				count := 0
				if during == "read" {
					raced.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
						if name == target.Name {
							count++
							if count == 2 {
								return appendObservation()
							}
						}
						return nil
					}}
				} else {
					native = func(ctx context.Context, plan tunnel.ReceiveTransitionPlan, view handover.View, uid string, routes tunnel.ReceiveJournalRoutes) error {
						if err := verify(ctx, plan, view, uid, routes); err != nil {
							return err
						}
						return appendObservation()
					}
				}
				before := calls
				if got, err := executeReceivePhase(ctx, api, raced, scope, cfg, site, "container", native); err != nil || len(got) != 1 || calls != before+1 {
					t.Fatalf("concurrent observation prevented execution or receipt: %v", err)
				}
			})
		}
	}
	for _, target := range []string{otherStore.Name, originalIndex.Name} {
		t.Run("changed-during-read-"+target, func(t *testing.T) {
			count := 0
			raced := store
			raced.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
				if name != target {
					return nil
				}
				count++
				if count != 2 {
					return nil
				}
				object, err := store.ConfigMaps.Get(ctx, target, metav1.GetOptions{})
				if err != nil {
					return err
				}
				object.Labels = map[string]string{"read-race": fmt.Sprint(time.Now().UnixNano())}
				_, err = store.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{})
				return err
			}}
			before := calls
			if got, err := executeReceivePhase(ctx, api, raced, scope, cfg, site, "container", verify); err == nil || len(got) != 0 || calls != before {
				t.Fatal("changing complete set permitted native work")
			}
		})
		t.Run("changed-during-native-"+target, func(t *testing.T) {
			native := func(ctx context.Context, plan tunnel.ReceiveTransitionPlan, view handover.View, uid string, routes tunnel.ReceiveJournalRoutes) error {
				if err := verify(ctx, plan, view, uid, routes); err != nil {
					return err
				}
				object, err := store.ConfigMaps.Get(ctx, target, metav1.GetOptions{})
				if err != nil {
					return err
				}
				object.Labels = map[string]string{"native-race": fmt.Sprint(time.Now().UnixNano())}
				_, err = store.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{})
				return err
			}
			before := calls
			if got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", native); err == nil || len(got) != 0 || calls != before+1 {
				t.Fatal("changed other worker or inventory produced receipts", err)
			}
		})
	}
	t.Run("inventory-shrink", func(t *testing.T) {
		current, err := store.ConfigMaps.Get(ctx, originalIndex.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		saved := current.DeepCopy()
		current.Data = originalIndex.Data
		if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		before := calls
		if got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", verify); err == nil || !strings.Contains(err.Error(), "regressed") || len(got) != 0 || calls != before {
			t.Fatal("shrunk inventory discarded another worker")
		}
		current, err = store.ConfigMaps.Get(ctx, originalIndex.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		current.Data = saved.Data
		if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	})
	for _, fault := range []string{"removed", "replaced"} {
		t.Run("retirement-intent-"+fault, func(t *testing.T) {
			path := lockPath + ".inventory.json"
			good, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.WriteFile(path, good, 0600); err != nil {
					t.Error(err)
				}
			})
			var saved receiveInventoryCheckpoint
			if err := json.Unmarshal(good, &saved); err != nil {
				t.Fatal(err)
			}
			publication := tunnel.ReceiveJournalPublication{JournalUID: string(firstSnapshot.Metadata().UID), Retirement: json.RawMessage(`{"intent":"retained"}`)}
			if saved.Inventory.Publications == nil {
				saved.Inventory.Publications = map[string]tunnel.ReceiveJournalPublication{}
			}
			saved.Inventory.Publications[first.MachineUID] = publication
			raw, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			v, object, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
			if err != nil {
				t.Fatal(err)
			}
			original := object.DeepCopy()
			publication.Retirement = nil
			if fault == "replaced" {
				publication.Retirement = json.RawMessage(`{"intent":"replacement"}`)
			}
			if v.Publications == nil {
				v.Publications = map[string]tunnel.ReceiveJournalPublication{}
			}
			v.Publications[first.MachineUID] = publication
			raw, err = json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			object.Data["inventory.json"] = string(raw)
			if _, err := store.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				current, err := store.ConfigMaps.Get(ctx, object.Name, metav1.GetOptions{})
				if err != nil {
					t.Error(err)
					return
				}
				current.Data = original.Data
				if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
					t.Error(err)
				}
			})
			before := calls
			if got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", verify); err == nil || !strings.Contains(err.Error(), "retirement intent") || len(got) != 0 || calls != before {
				t.Fatal("retirement regression permitted native work", err)
			}
		})
	}
	t.Run("corrupt-inventory-checkpoint", func(t *testing.T) {
		path := lockPath + ".inventory.json"
		good, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		before := calls
		if got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", verify); err == nil || len(got) != 0 || calls != before {
			t.Fatal("corrupt checkpoint permitted live fallback")
		}
		if err := os.WriteFile(path, good, 0600); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unindexed-local-checkpoint", func(t *testing.T) {
		path := lockPath + "." + tunnel.ReceiveTransitionName("omitted-worker") + ".json"
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(path)
		before := calls
		if got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", verify); err == nil || !strings.Contains(err.Error(), "omitted a checkpointed") || len(got) != 0 || calls != before {
			t.Fatal("unindexed local journal permitted native work", err)
		}
	})
	for _, offset := range []time.Duration{-2 * time.Minute, time.Minute} {
		t.Run("other-worker-not-executable-"+offset.String(), func(t *testing.T) {
			current, err := store.ConfigMaps.Get(ctx, otherStore.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			good := current.DeepCopy()
			tr, err := handover.NewTransition(spec, time.Now().Add(offset), handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := tr.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			current.Data["transition.json"] = string(raw)
			if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			fresh := cfg
			fresh.privateKeyFile = filepath.Join(t.TempDir(), "key")
			before := calls
			if got, err := executeReceivePhase(ctx, api, store, scope, fresh, site, "container", verify); err == nil || !strings.Contains(err.Error(), "another local handover") || len(got) != 0 || calls != before {
				t.Fatal("inactive other worker allowed combined route execution", err)
			}
			// A completed target must not borrow mutation authority from an
			// expired or future unfinished worker in the shared route set.
			target, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			terminal, err := target.Transition().Rollback(time.Now())
			if err != nil {
				t.Fatal(err)
			}
			v, err := terminal.View()
			if err != nil {
				t.Fatal(err)
			}
			digest, err := v.Round.Digest()
			if err != nil {
				t.Fatal(err)
			}
			var proofs []handover.AuthenticatedReceipt
			for _, member := range v.Round.Members {
				proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: member.NodeUID, Receipt: handover.Receipt{RoundHash: digest, NodeUID: member.NodeUID, DeviceID: member.DeviceID, FromKey: member.FromKey, ToKey: member.ToKey, FromPeerKey: member.FromPeerKey, ToPeerKey: member.ToPeerKey, ObservedAt: time.Now()}})
			}
			terminal, err = terminal.Advance(time.Now(), proofs)
			if err != nil {
				t.Fatal(err)
			}
			terminalRaw, err := terminal.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			targetCM, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			targetData := targetCM.DeepCopy().Data
			targetCM.Data["transition.json"] = string(terminalRaw)
			if _, err := store.ConfigMaps.Update(ctx, targetCM, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			maintenanceCfg := cfg
			maintenanceCfg.privateKeyFile = filepath.Join(t.TempDir(), "key")
			invoked := false
			native := func(context.Context, tunnel.ReceiveTransitionPlan, handover.View, string, tunnel.ReceiveJournalRoutes) error {
				invoked = true
				return nil
			}
			if err := maintainCompletedReceivePhase(ctx, api, store, scope, maintenanceCfg, site, "container", native); err == nil || !strings.Contains(err.Error(), "another local handover") || invoked {
				t.Fatal("terminal maintenance borrowed inactive worker authority", err)
			}
			if err := verifyCompletedReceivePhase(ctx, api, store, scope, maintenanceCfg, site, "container", native); err != nil || !invoked {
				t.Fatal("read-only terminal verification was blocked by inactive worker", err)
			}
			targetCM, err = store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			targetCM.Data = targetData
			if _, err := store.ConfigMaps.Update(ctx, targetCM, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			current, err = store.ConfigMaps.Get(ctx, otherStore.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			current.Data = good.Data
			if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("nonlocal-successor-history", func(t *testing.T) {
		otherScope := scope
		otherScope.MachineUID = other.MachineUID
		verifyFreshReceiveHistory(t, api, otherStore, otherScope, otherKey, func(candidate handover.Store) error {
			firstStore := store
			firstStore.ConfigMaps = candidate.ConfigMaps
			_, err := loadReceiveSet(ctx, api, firstStore, scope, first.CloudNodeName, firstRoot)
			return err
		})
	})
	t.Run("replaced-inventory", func(t *testing.T) {
		current, err := store.ConfigMaps.Get(ctx, originalIndex.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.ConfigMaps.Delete(ctx, current.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		current.UID, current.ResourceVersion = "", ""
		if _, err := store.ConfigMaps.Create(ctx, current, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		before := calls
		if got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", verify); err == nil || !strings.Contains(err.Error(), "checkpoint identity changed") || len(got) != 0 || calls != before {
			t.Fatal("replacement inventory inherited durable admission", err)
		}
	})
	t.Run("survivor-during-retirement", func(t *testing.T) {
		verifySurvivorReceiveAdmission(t, api, store, scope, otherStore, site, firstRoot)
	})

}
