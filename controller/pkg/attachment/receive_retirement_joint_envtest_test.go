package attachment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Uses the existing real TokenRequest/TokenReview identity fixture and real API
// CAS. Serving history and native observations are synthetic. This qualifies
// joint evidence admission/finalization, not kernel cleanup or live continuity.
func verifyJointRetirementAuthentication(t *testing.T, api kubernetes.Interface, policy handover.PodReceiptAuthenticator, nodes []*corev1.Node, entries []handover.PodReceipt, history string) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ns := policy.Namespace
	mesh, err := api.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh"}}, metav1.CreateOptions{})
	must(err)
	maps := api.CoreV1().ConfigMaps(ns)
	names := []string{}
	defer func() {
		for _, name := range names {
			must(maps.Delete(ctx, name, metav1.DeleteOptions{}))
		}
		must(api.CoreV1().Secrets(ns).Delete(ctx, mesh.Name, metav1.DeleteOptions{}))
	}()
	inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: "cluster", Namespace: ns, MeshName: mesh.Name, MeshUID: string(mesh.UID), Publications: map[string]tunnel.ReceiveJournalPublication{}}
	intents := []RemoteWithdrawalIntent{}
	fixtures := []handover.Snapshot{}
	plans := []tunnel.ReceiveTransitionPlan{}
	stores := []handover.Store{}
	snapshots := []handover.Snapshot{}
	mesh.Data = map[string][]byte{}
	for _, seed := range []byte{4, 6} {
		w, fixture, _, _ := retirementFixtureForConsumer(t, history, true, string(mesh.UID), ns, nodes[0].Name, string(nodes[0].UID), seed)
		plan, err := tunnel.DecodeReceiveTransition(fixture, "cluster")
		must(err)
		mesh.Data[tunnel.NodePublicKeyPrefix+nodes[0].Name] = []byte(w.Consumers[0].PublicKey)
		mesh.Data[tunnel.PeerPublicKeyPrefix+w.MachineName] = []byte(w.PublicKey)
		intents, fixtures, plans = append(intents, w), append(fixtures, fixture), append(plans, plan)
	}
	mesh, err = api.CoreV1().Secrets(ns).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	for i := range intents {
		other := plans[1-i]
		intents[i].SourceVersion = mesh.ResourceVersion
		intents[i].SourceDataHash = withdrawalSourceDataHash(mesh.Data)
		intents[i].Consumers = append(intents[i].Consumers,
			ConsumerTarget{Site: true, NodeName: nodes[1].Name, NodeUID: string(nodes[1].UID)},
			ConsumerTarget{NodeName: other.CloudNodeName, NodeUID: other.CloudNodeUID, MachineName: other.MachineName, SecretUID: other.AdoptionUID, PublicKey: other.RootPublicKey, TunnelAddress: fmt.Sprintf("172.29.0.%d", []byte{4, 6}[1-i])})
		raw, err := fixtures[i].Checkpoint()
		must(err)
		var checkpoint struct {
			Object corev1.ConfigMap `json:"object"`
		}
		must(json.Unmarshal(raw, &checkpoint))
		checkpoint.Object.UID, checkpoint.Object.ResourceVersion = "", ""
		if strings.HasPrefix(history, "interrupted-") {
			checkpoint.Object.Data["withdrawal-intent-sha256"], err = intents[i].withdrawalIntentHash()
			must(err)
		}
		journal, err := maps.Create(ctx, &checkpoint.Object, metav1.CreateOptions{})
		must(err)
		names = append(names, journal.Name)
		store := handover.Store{ConfigMaps: maps, ClusterUID: "cluster", Name: journal.Name, ExpectedUID: string(journal.UID)}
		snapshot, err := store.Load(ctx)
		must(err)
		stores, snapshots = append(stores, store), append(snapshots, snapshot)
		inventory.MachineUIDs = append(inventory.MachineUIDs, intents[i].MachineUID)
		inventory.Publications[intents[i].MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(journal.UID)}
	}
	raw, err := json.Marshal(inventory)
	must(err)
	index, err := maps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveInventoryName(string(mesh.UID))}, Data: map[string]string{"inventory.json": string(raw)}}, metav1.CreateOptions{})
	must(err)
	names = append(names, index.Name)
	for i, w := range intents {
		must(w.ReserveReceiveWithdrawal(ctx, maps, "cluster", string(index.UID), mesh, snapshots[i]))
	}
	for _, w := range intents {
		mesh.Data, _, err = tunnel.WithdrawRemotePeer(mesh.Data, w.MachineName, w.PublicKey)
		must(err)
	}
	mesh, err = api.CoreV1().Secrets(ns).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	policies := map[string]handover.PodReceiptAuthenticator{string(nodes[0].UID): policy, string(nodes[1].UID): policy}
	t.Run("shared-binding-read-budget", func(t *testing.T) {
		source, err := ReadReceiveRetirementSet(ctx, api, stores[0], ns, mesh.Name, string(mesh.UID), intents[0].MachineUID)
		if err != nil {
			t.Fatal(err)
		}
		reads := map[string]int{}
		var readMu sync.Mutex
		reader := stores[0]
		reader.ConfigMaps = retirementCollectionReader{ConfigMapInterface: maps, before: func(name string) error { readMu.Lock(); defer readMu.Unlock(); reads[name]++; return nil }}
		collection, err := source.ReceiptCollection(ctx, api, reader, policies, time.Minute, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{stores[0].Name, stores[1].Name, index.Name} {
			if reads[name] != 1 {
				t.Errorf("source %s read %d times, want one live recheck", name, reads[name])
			}
		}
		standalone, err := NewReceiveRetirementReceiptScope(ctx, maps, "cluster", string(index.UID), snapshots[0], mesh)
		if err != nil {
			t.Fatal(err)
		}
		if collection.Digest != standalone.Digest() || len(collection.Members) != 2 {
			t.Fatal("shared binding differs from standalone admission")
		}
		// Reusing admitted data must not allow a missing sibling to escape its
		// live revision check or shrink the cleanup obligation.
		delete(source.Journals, intents[1].MachineUID)
		if _, err := source.ReceiptScope(ctx, stores[0]); err == nil {
			t.Fatal("incomplete admitted source produced a receipt scope")
		}
	})
	for i, store := range stores {
		t.Run(fmt.Sprintf("member-%d", i), func(t *testing.T) {
			scope, err := NewReceiveRetirementReceiptScope(ctx, maps, "cluster", string(index.UID), snapshots[i], mesh)
			must(err)
			if scope.version != receiveRetirementCohortCompletionVersion || len(scope.nodes) != 2 || !slices.Equal(scope.CleanupMachineUIDs(), inventory.MachineUIDs) {
				t.Fatal("incorrect joint consumer or cleanup scope")
			}
			fresh := func() []handover.PodReceipt {
				batch := slices.Clone(entries)
				for n := range batch {
					batch[n].Receipt.RoundHash = scope.Digest()
					batch[n].Receipt.ObservedAt = time.Now()
				}
				return batch
			}
			for _, fault := range []string{"missing-survivor", "single-worker-digest", "other-member-digest", "expired", "bad-token", "old-container", "cancelled"} {
				t.Run(fault, func(t *testing.T) {
					batch := fresh()
					runCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					switch fault {
					case "missing-survivor":
						batch = batch[:1]
					case "single-worker-digest":
						digest, e := receiveRetirementAuthorityDigest("cluster", string(index.UID), scope.inventoryHash, scope.inventory, mesh, scope.intent)
						must(e)
						for n := range batch {
							batch[n].Receipt.RoundHash = digest
						}
					case "other-member-digest":
						other, e := NewReceiveRetirementReceiptScope(ctx, maps, "cluster", string(index.UID), snapshots[1-i], mesh)
						must(e)
						for n := range batch {
							batch[n].Receipt.RoundHash = other.Digest()
						}
					case "expired":
						batch[0].Receipt.ObservedAt = time.Now().Add(-2 * time.Minute)
					case "bad-token":
						batch[0].BearerToken = "invalid"
					case "old-container":
						batch[0].Receipt.AgentContainerID = "containerd://old"
					case "cancelled":
						cancel()
					}
					before, e := maps.Get(ctx, index.Name, metav1.GetOptions{})
					must(e)
					if e := RecordReceiveRetirementWithPodReceipts(runCtx, api, store, string(index.UID), scope.Digest(), mesh, snapshots[i], batch, policies, time.Minute, func() error { return nil }); e == nil {
						t.Fatal("invalid joint evidence committed")
					}
					after, e := maps.Get(ctx, index.Name, metav1.GetOptions{})
					must(e)
					if before.ResourceVersion != after.ResourceVersion {
						t.Fatal("rejected evidence mutated inventory")
					}
				})
			}
			if i == 0 {
				t.Run("sibling-commit-conflict", func(t *testing.T) {
					verifyRetirementCommitConflictRetry(t, api, stores, snapshots, mesh, string(index.UID), policies, fresh())
				})
			}
			must(RecordReceiveRetirementWithPodReceipts(ctx, api, store, string(index.UID), scope.Digest(), mesh, snapshots[i], fresh(), policies, time.Minute, func() error { return nil }))
			source, ok, err := ReadVerifiedReceiveRetirementCompletion(ctx, api, store, ns, mesh.Name, string(mesh.UID), intents[i].MachineUID, string(index.UID), time.Minute)
			must(err)
			if !ok || len(source.Withdrawal.Consumers) != 3 {
				t.Fatal("joint evidence lost original captured consumers")
			}
			var completion receiveRetirementCompletion
			must(json.Unmarshal(source.Inventory.Publications[intents[i].MachineUID].RetirementCompletion, &completion))
			if completion.Version != 5 || len(completion.Receipts) != 2 || !slices.Equal(completion.Cohort, inventory.MachineUIDs) {
				t.Fatal("persisted completion lost joint binding")
			}
		})
	}
	for i, store := range stores {
		if i > 0 {
			// Finalization changes authority. The next member needs freshly
			// authenticated observations, not a rebase of its earlier batch.
			if err := FinalizeReceiveRetirementCompletion(ctx, api, store, ns, mesh.Name, string(mesh.UID), intents[i].MachineUID, string(index.UID), time.Minute, func() error { return nil }); err == nil {
				t.Fatal("sibling finalization silently rebased old completion")
			}
			scope, err := NewReceiveRetirementReceiptScope(ctx, maps, "cluster", string(index.UID), snapshots[i], mesh)
			must(err)
			batch := slices.Clone(entries)
			for n := range batch {
				batch[n].Receipt.RoundHash = scope.Digest()
				batch[n].Receipt.ObservedAt = time.Now()
			}
			must(RecordReceiveRetirementWithPodReceipts(ctx, api, store, string(index.UID), scope.Digest(), mesh, snapshots[i], batch, policies, time.Minute, func() error { return nil }))
		}
		must(FinalizeReceiveRetirementCompletion(ctx, api, store, ns, mesh.Name, string(mesh.UID), intents[i].MachineUID, string(index.UID), time.Minute, func() error { return nil }))
	}
	for i, store := range stores {
		source, ok, err := ReadReceiveRetirementFinalization(ctx, api, store, ns, mesh.Name, string(mesh.UID), intents[i].MachineUID, string(index.UID), time.Minute)
		must(err)
		if !ok {
			t.Fatal("joint finalization did not verify")
		}
		// Historical verification must survive subsequent mesh ownership. This
		// private projection is not live mutation or allocation authority.
		historical := source
		historical.Mesh = source.Mesh.DeepCopy()
		historical.Mesh.ResourceVersion = "later-mesh-version"
		historical.Mesh.Data[tunnel.PeerPublicKeyPrefix+intents[i].MachineName] = []byte(intents[1-i].PublicKey)
		historical.MeshMetadata = historical.Mesh.ObjectMeta
		ok, err = validateReceiveRetirementFinalization(historical, "cluster", intents[i].MachineUID, string(index.UID), time.Minute, time.Now())
		must(err)
		if !ok {
			t.Fatal("later mesh ownership invalidated consumed history")
		}
		if _, err := historical.RetirementCohort(); err == nil {
			t.Fatal("historical evidence authorized cleanup of a replacement owner")
		}
		missing := historical
		missing.Journals = map[string]handover.Snapshot{intents[i].MachineUID: snapshots[i]}
		if ok, err := validateReceiveRetirementFinalization(missing, "cluster", intents[i].MachineUID, string(index.UID), time.Minute, time.Now()); err == nil || ok {
			t.Fatal("joint historical verification omitted a member journal")
		}
		before, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
		must(err)
		err = ReleaseFinalizedReceiveRetirement(ctx, api, store, ns, mesh.Name, string(mesh.UID), intents[i].MachineUID, string(index.UID), time.Minute, func(ReceiveRetirementSet) error { t.Fatal("individual joint release reached policy"); return nil })
		if err == nil || !strings.Contains(err.Error(), "atomic cohort release") {
			t.Fatalf("joint release guard not exercised: %v", err)
		}
		after, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
		must(err)
		if before.ResourceVersion != after.ResourceVersion {
			t.Fatal("individual release changed joint ownership")
		}
	}
	verifyJointRetirementRelease(t, api, stores[0], mesh, intents[0].MachineUID, policies, entries)
	retained, _, err := tunnel.ReadReceiveInventory(ctx, maps, "cluster", ns, mesh.Name, string(mesh.UID))
	must(err)
	raw, err = json.Marshal(retained)
	must(err)
	mesh.Data[tunnel.PeerPublicKeyPrefix+intents[0].MachineName] = []byte(intents[1].PublicKey)
	mesh, err = api.CoreV1().Secrets(ns).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	for i, store := range stores {
		released, ok, err := ReadReleasedReceiveJournal(ctx, api, store, ns, mesh.Name, string(mesh.UID), intents[i].MachineUID, string(index.UID), time.Minute)
		must(err)
		if !ok || len(released.source.Journals) != 2 || len(released.Inventory.MachineUIDs) != 0 || len(released.Inventory.Publications) != 0 {
			t.Fatal("released group omitted retained history or exposed active authority")
		}
		must(released.Recheck(ctx, api, store))
	}
	// Shared admission must still reread every journal and fence the live
	// source. The read budget checks duplicate I/O, not elapsed-time behavior.
	counts := map[string]int{}
	var countMu sync.Mutex
	counted := stores[0]
	parallelCtx, parallelCancel := context.WithTimeout(ctx, 15*time.Second)
	defer parallelCancel()
	bothReading := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	firstReads := [2]int{}
	counted.ConfigMaps = retirementCollectionReader{ConfigMapInterface: maps, before: func(name string) error {
		countMu.Lock()
		counts[name]++
		phase := counts[name] - 1
		initialJournal := (name == stores[0].Name || name == stores[1].Name) && phase < 2
		if initialJournal {
			firstReads[phase]++
			if firstReads[phase] == 2 {
				close(bothReading[phase])
			}
		}
		countMu.Unlock()
		if initialJournal {
			select {
			case <-bothReading[phase]:
			case <-parallelCtx.Done():
				return parallelCtx.Err()
			}
		}
		return nil
	}}
	group, err := ReadReleasedReceiveJournals(parallelCtx, api, counted, ns, mesh.Name, string(mesh.UID), string(index.UID), time.Minute)
	must(err)
	parallelCancel()
	counted.ConfigMaps = retirementCollectionReader{ConfigMapInterface: maps, before: func(name string) error {
		countMu.Lock()
		defer countMu.Unlock()
		counts[name]++
		return nil
	}}

	if len(group.Records) != 2 || counts[index.Name] != 2 {
		t.Fatalf("joint history admission did not share inventory reads: %v", counts)
	}
	for i, store := range stores {
		record := group.Records[intents[i].MachineUID]
		if counts[store.Name] != 2 || record.Journal.Metadata().UID != snapshots[i].Metadata().UID || len(record.Inventory.Publications) != 0 {
			t.Fatalf("joint history omitted a journal or repeated its reads: %v", counts)
		}
	}
	// Cancellation during independent history reads must return no records and
	// join both callbacks before returning to the caller.
	t.Run("shared-history-cancellation", func(t *testing.T) {
		cancelCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		var mu sync.Mutex
		active, entered := 0, 0
		reader := stores[0]
		reader.ConfigMaps = retirementCollectionReader{ConfigMapInterface: maps, before: func(name string) error {
			if name != stores[0].Name && name != stores[1].Name {
				return nil
			}
			mu.Lock()
			active++
			entered++
			if entered == 2 {
				cancel()
			}
			mu.Unlock()
			<-cancelCtx.Done()
			mu.Lock()
			active--
			mu.Unlock()
			return cancelCtx.Err()
		}}
		rejected, err := ReadReleasedReceiveJournals(cancelCtx, api, reader, ns, mesh.Name, string(mesh.UID), string(index.UID), time.Minute)
		if !errors.Is(err, context.Canceled) || len(rejected.Records) != 0 || active != 0 || entered != 2 {
			t.Fatalf("cancelled shared history returned partial or unjoined admission: err=%v records=%d active=%d entered=%d", err, len(rejected.Records), active, entered)
		}
	})
	// This fixture explicitly finalized with a one-minute retirement policy,
	// independently of its shorter serving-round policy. Deriving the latter
	// must reject that mismatch, rather than trusting the evidence's age field.
	for _, age := range []time.Duration{0, -time.Second} {
		rejected, err := ReadReleasedReceiveJournals(ctx, api, stores[0], ns, mesh.Name, string(mesh.UID), string(index.UID), age)
		if err == nil || len(rejected.Records) != 0 {
			t.Fatal("shared history accepted a different observation lifetime")
		}
	}
	clear(counts)
	must(group.Recheck(ctx, api, counted))
	if counts[index.Name] != 1 || counts[stores[0].Name] != 1 || counts[stores[1].Name] != 1 {
		t.Fatalf("shared recheck must read each source once: %v", counts)
	}
	changed := false
	clear(counts)
	counted.ConfigMaps = retirementCollectionReader{ConfigMapInterface: maps, before: func(name string) error {
		countMu.Lock()
		defer countMu.Unlock()
		counts[name]++
		if name != index.Name || counts[name] != 2 {
			return nil
		}
		current, err := api.CoreV1().Secrets(ns).Get(ctx, mesh.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations["test-revision"] = "changed-during-shared-admission"
		_, err = api.CoreV1().Secrets(ns).Update(ctx, current, metav1.UpdateOptions{})
		changed = err == nil
		return err
	}}
	rejected, err := ReadReleasedReceiveJournals(ctx, api, counted, ns, mesh.Name, string(mesh.UID), string(index.UID), time.Minute)
	if !changed || !apierrors.IsConflict(err) || len(rejected.Records) != 0 {
		t.Fatalf("shared history admission must reject a changed revision with a conflict: %v", err)
	}
	if err := group.Recheck(ctx, api, stores[0]); err == nil {
		t.Fatal("shared recheck reused a cached mesh revision")
	}
	// A rejected snapshot must be discarded. A fresh admission can converge
	// after an unrelated source update without rewriting history or making
	// the old snapshot valid. This is a real-API consistency test, not proof
	// that a live verifier will finish within its observation deadline.
	beforeMesh, err := api.CoreV1().Secrets(ns).Get(ctx, mesh.Name, metav1.GetOptions{})
	must(err)
	beforeIndex, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
	must(err)
	fresh, err := ReadReleasedReceiveJournals(ctx, api, stores[0], ns, mesh.Name, string(mesh.UID), string(index.UID), time.Minute)
	must(err)
	if len(fresh.Records) != len(group.Records) {
		t.Fatal("fresh admission lost released history")
	}
	for uid, old := range group.Records {
		current := fresh.Records[uid]
		if current.Journal.Metadata().UID != old.Journal.Metadata().UID ||
			current.Journal.Metadata().ResourceVersion != old.Journal.Metadata().ResourceVersion ||
			!reflect.DeepEqual(current.Inventory.Released[uid], old.Inventory.Released[uid]) ||
			!reflect.DeepEqual(current.Plan, old.Plan) || !reflect.DeepEqual(current.Withdrawal, old.Withdrawal) {
			t.Fatal("fresh admission changed retained journal or consumed evidence")
		}
	}
	must(fresh.Recheck(ctx, api, stores[0]))
	for _, fault := range []string{"mesh-replacement", "inventory-replacement"} {
		t.Run("source-conflict/"+fault, func(t *testing.T) {
			invalid := fresh
			if fault == "mesh-replacement" {
				invalid.source.MeshMetadata.UID = "replaced"
				invalid.source.Metadata.ResourceVersion = "stale"
			} else {
				invalid.source.Metadata.UID = "replaced"
				invalid.source.MeshMetadata.ResourceVersion = "stale"
			}
			if err := invalid.Recheck(ctx, api, stores[0]); err == nil || apierrors.IsConflict(err) {
				t.Fatalf("concurrent revision hid source identity failure: %v", err)
			}
		})
	}
	if err := group.Recheck(ctx, api, stores[0]); err == nil {
		t.Fatal("fresh admission repaired stale evidence")
	}
	afterMesh, err := api.CoreV1().Secrets(ns).Get(ctx, mesh.Name, metav1.GetOptions{})
	must(err)
	afterIndex, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
	must(err)
	if beforeMesh.ResourceVersion != afterMesh.ResourceVersion || beforeIndex.ResourceVersion != afterIndex.ResourceVersion {
		t.Fatal("historical read mutated its source")
	}
	for _, fault := range []string{"missing-member", "replaced-journal", "mismatched-cohort"} {
		t.Run("released-history/"+fault, func(t *testing.T) {
			var damaged tunnel.ReceiveInventory
			must(json.Unmarshal(raw, &damaged))
			uid := intents[1].MachineUID
			pub := damaged.Released[uid]
			switch fault {
			case "missing-member":
				delete(damaged.Released, uid)
			case "replaced-journal":
				pub.JournalUID = "replacement"
				damaged.Released[uid] = pub
			case "mismatched-cohort":
				var completion receiveRetirementCompletion
				must(json.Unmarshal(pub.RetirementCompletion, &completion))
				completion.Cohort = []string{uid}
				pub.RetirementCompletion, err = json.Marshal(completion)
				must(err)
				damaged.Released[uid] = pub
			}
			encoded, err := json.Marshal(damaged)
			must(err)
			current, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
			must(err)
			current.Data["inventory.json"] = string(encoded)
			_, err = maps.Update(ctx, current, metav1.UpdateOptions{})
			must(err)
			if _, ok, err := ReadReleasedReceiveJournal(ctx, api, stores[0], ns, mesh.Name, string(mesh.UID), intents[0].MachineUID, string(index.UID), time.Minute); err == nil || ok {
				t.Fatal("damaged joint history accepted")
			}
			if damaged, err := ReadReleasedReceiveJournals(ctx, api, stores[0], ns, mesh.Name, string(mesh.UID), string(index.UID), time.Minute); err == nil || len(damaged.Records) != 0 {
				t.Fatal("shared admission accepted damaged joint history")
			}
			current, err = maps.Get(ctx, index.Name, metav1.GetOptions{})
			must(err)
			current.Data["inventory.json"] = string(raw)
			_, err = maps.Update(ctx, current, metav1.UpdateOptions{})
			must(err)
		})
	}

}
