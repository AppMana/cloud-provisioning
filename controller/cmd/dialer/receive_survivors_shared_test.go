package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Two withdrawn histories and one serving journal, using real API storage.
// Terminal proofs are synthetic; absent owner objects do not establish actual
// deletion, native absence, authenticated completion or live admission latency.
func verifySharedSurvivorRetirements(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, retiring handover.Store, plan tunnel.ReceiveTransitionPlan, withdrawal attachment.RemoteWithdrawalIntent, key, cloudKey wgtypes.Key) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	must(err)
	originalIndex := index.DeepCopy()
	mesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
	must(err)
	originalMesh := mesh.DeepCopy()
	thirdKey, err := wgtypes.GeneratePrivateKey()
	must(err)
	raw, err := json.Marshal(plan)
	must(err)
	var third tunnel.ReceiveTransitionPlan
	must(json.Unmarshal(raw, &third))
	third.MachineName, third.MachineUID = "worker-three", "third-machine-uid"
	third.CloudNodeName, third.CloudNodeUID = "cloud-three", "third-cloud-uid"
	third.AdoptionName, third.AdoptionUID = tunnel.AdoptionSecretName(third.MachineName), "third-adoption-uid"
	third.RootPublicKey = thirdKey.PublicKey().String()
	binding := third.Bindings[key.PublicKey().String()]
	binding.PublicKey = receiverKey(thirdKey, scope.MeshUID, key.PublicKey().String()).PublicKey().String()
	third.Bindings[key.PublicKey().String()] = binding
	third.SiteDesired = tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: third.RootPublicKey, Remote: true, Endpoint: "198.51.100.3:51820", WGAllowedIPs: []string{"172.29.0.3/32"}}}}
	site := third.Sites[key.PublicKey().String()]
	site.Client, err = tunnel.PlanSiteReceiveClient(scope.MeshUID, key.PublicKey().String(), third.RootPublicKey, binding, third.SiteDesired, third.SiteDesired)
	must(err)
	third.Sites[key.PublicKey().String()] = site
	thirdStore := store
	thirdStore.Name, thirdStore.ExpectedUID = tunnel.ReceiveTransitionName(third.MachineUID), ""
	must(tunnel.ReserveReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, third.MachineUID, store.OwnerReferences))
	t.Cleanup(func() {
		current, err := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
		must(err)
		current.Data = originalIndex.Data
		_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
		must(err)
		must(store.ConfigMaps.Delete(ctx, thirdStore.Name, metav1.DeleteOptions{}))
		currentMesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
		must(err)
		currentMesh.Data = originalMesh.Data
		_, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, currentMesh, metav1.UpdateOptions{})
		must(err)
	})
	raw, err = json.Marshal(third)
	must(err)
	spec, err := tunnel.ReceiveTransitionSpec(third, store.ClusterUID, fmt.Sprintf("%x", sha256.Sum256(raw)))
	must(err)
	transition, err := handover.NewTransition(spec, time.Now(), handover.Policy{RoundLifetime: 45 * time.Second, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
	must(err)
	_, err = thirdStore.CreateWithPlan(ctx, transition, raw)
	must(err)
	snapshot := completeReceiveTestJournal(t, thirdStore)
	snapshot, err = thirdStore.FreezeCompleted(ctx, snapshot)
	must(err)
	inventory, index, err = tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	must(err)
	inventory.Publications[third.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(snapshot.Metadata().UID)}
	raw, err = json.Marshal(inventory)
	must(err)
	index.Data["inventory.json"] = string(raw)
	index, err = store.ConfigMaps.Update(ctx, index, metav1.UpdateOptions{})
	must(err)
	mesh.Data[tunnel.PeerPublicKeyPrefix+third.MachineName] = []byte(third.RootPublicKey)
	mesh, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	withdrawal.MachineName, withdrawal.MachineUID, withdrawal.PublicKey, withdrawal.SourceVersion = third.MachineName, third.MachineUID, third.RootPublicKey, mesh.ResourceVersion
	must(withdrawal.ReserveReceiveWithdrawal(ctx, store.ConfigMaps, store.ClusterUID, string(index.UID), mesh, snapshot))
	mesh.Data, _, err = tunnel.WithdrawRemotePeer(mesh.Data, third.MachineName, third.RootPublicKey)
	must(err)
	_, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	reads := 0
	reader := store
	reader.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
		if name == retiring.Name {
			reads++
		}
		return nil
	}}
	admitted, err := loadReceiveSet(ctx, api, reader, scope, "site", key)
	must(err)
	if len(admitted.Retirements) != 2 || len(admitted.Journals) != 3 || len(admitted.Routes.Plans) != 1 {
		t.Fatal("shared admission lost reserved or serving membership")
	}
	if reads > 5 {
		t.Fatalf("duplicate full-set admission: %d retained root reads, budget 5", reads)
	}
	serving, err := store.Load(ctx)
	must(err)
	servingPlan, err := tunnel.DecodeReceiveTransition(serving, store.ClusterUID)
	must(err)
	for _, fault := range []string{"consumer-replaced", "consumer-omitted", "malformed", "source-race"} {
		t.Run(fault, func(t *testing.T) {
			inventory, original, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
			must(err)
			t.Cleanup(func() {
				current, err := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
				must(err)
				current.Data, current.Annotations = original.Data, original.Annotations
				_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
				must(err)
			})
			reader := store
			if fault == "source-race" {
				reads := 0
				reader.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
					if name != thirdStore.Name {
						return nil
					}
					reads++
					if reads != 4 {
						return nil
					}
					current, err := store.ConfigMaps.Get(ctx, original.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if current.Annotations == nil {
						current.Annotations = map[string]string{}
					}
					current.Annotations["test-shared-race"] = "changed"
					_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
					return err
				}}
				if _, err := loadReceiveSet(ctx, api, reader, scope, servingPlan.CloudNodeName, cloudKey); err == nil || reads < 4 {
					t.Fatal("shared admission missed source mutation", err)
				}
				return
			}
			publication := inventory.Publications[third.MachineUID]
			if fault == "malformed" {
				publication.Retirement = json.RawMessage(`{}`)
			} else {
				var record struct {
					Version       int                               `json:"version"`
					JournalSHA256 string                            `json:"journalSHA256"`
					Withdrawal    attachment.RemoteWithdrawalIntent `json:"withdrawal"`
				}
				must(json.Unmarshal(publication.Retirement, &record))
				var consumers []attachment.ConsumerTarget
				changed := false
				for _, consumer := range record.Withdrawal.Consumers {
					if consumer.NodeName == servingPlan.CloudNodeName {
						changed = true
						if fault == "consumer-omitted" {
							continue
						}
						consumer.NodeUID = "replacement-cloud-uid"
					}
					consumers = append(consumers, consumer)
				}
				if !changed {
					t.Fatal("missing cloud consumer fixture")
				}
				record.Withdrawal.Consumers = consumers
				publication.Retirement, err = json.Marshal(record)
				must(err)
			}
			inventory.Publications[third.MachineUID] = publication
			raw, err := json.Marshal(inventory)
			must(err)
			current := original.DeepCopy()
			current.Data["inventory.json"] = string(raw)
			_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
			must(err)
			if _, err := loadReceiveSet(ctx, api, reader, scope, servingPlan.CloudNodeName, cloudKey); err == nil {
				t.Fatal("invalid sibling withdrawal inherited admission")
			}
		})
	}
	t.Run("checkpoint-shares-released-admission", func(t *testing.T) {
		verifySharedReleasedCheckpoint(t, api, store, scope, retiring, thirdStore, key)
	})
	t.Run("released-sibling-during-retirement", func(t *testing.T) {
		inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
		must(err)
		p := inventory.Publications[third.MachineUID]
		// Synthetic completion is sufficient only for exclusion planning and
		// mutation guards; it cannot authorize a checkpoint or native work.
		p.RetirementCompletion, p.RetirementFinalization = json.RawMessage(`{}`), json.RawMessage(`{}`)
		inventory.Released = map[string]tunnel.ReceiveJournalPublication{third.MachineUID: p}
		delete(inventory.Publications, third.MachineUID)
		inventory.MachineUIDs = nil
		for _, uid := range []string{plan.MachineUID, scope.MachineUID} {
			inventory.MachineUIDs = append(inventory.MachineUIDs, uid)
		}
		slices.Sort(inventory.MachineUIDs)
		raw, err := json.Marshal(inventory)
		must(err)
		index.Data["inventory.json"] = string(raw)
		_, err = store.ConfigMaps.Update(ctx, index, metav1.UpdateOptions{})
		must(err)
		admitted, err := loadReceiveSet(ctx, api, store, scope, "site", key)
		must(err)
		retired := admitted.Retirements[plan.MachineUID]
		if len(admitted.ReleasedJournals) != 1 || len(retired.ReleasedJournals) != 1 || retired.Routes.Serving.Withdrawn[third.MachineUID] != third.RootPublicKey || retired.Routes.Reserved.Withdrawn[third.MachineUID] != third.RootPublicKey {
			t.Fatal("overlapping retirement lost released history or root exclusion")
		}
		retiredScope := scope
		retiredScope.MachineUID = plan.MachineUID
		verifyReleasedJournalExecutionGuard(t, thirdStore, func() error {
			return recheckReceiveSet(ctx, api, store, scope, "site", key, admitted)
		})
		retired, err = loadReceiveRetirement(ctx, api, retiring, retiredScope, "site", key)
		must(err)
		verifyReleasedJournalExecutionGuard(t, thirdStore, func() error {
			return recheckReceiveRetirement(ctx, api, retiring, retired, "site", key)
		})
	})

}

// Real API storage with synthetic legacy finalization and no native callback.
// This exercises shared checkpoint admission, not cleanup, release policy or
// reboot recovery. Reusing history must retain binding and live source fences.
func verifySharedReleasedCheckpoint(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, retiring, releasedStore handover.Store, key wgtypes.Key) {
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := releasedStore.Load(ctx)
	must(err)
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	must(err)
	releasedScope := scope
	releasedScope.MachineUID = plan.MachineUID
	seedReceiveFinalization(t, api, releasedStore, releasedScope, false)
	cfg := config{iface: "sharedrelease", nodeName: "site", privateKeyFile: filepath.Join(t.TempDir(), "key")}
	lock, err := lockReconciliation(cfg)
	must(err)
	defer func() { lock.Close() }()
	set, err := loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key)
	must(err)
	must(checkpointReceiveSetLocked(ctx, api, store, scope, cfg, key, lock.Name(), set))
	source := set.Retirements[plan.MachineUID]
	must(tunnel.ReleaseReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, source.Metadata, source.Mesh, source.Journals[plan.MachineUID], source.Inventory.Publications[plan.MachineUID], func() error { return nil }))
	set, err = loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key)
	must(err)
	reads := 0
	reader := store
	reader.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
		if name == releasedStore.Name {
			reads++
		}
		return nil
	}}
	must(checkpointReceiveSetLocked(ctx, api, reader, scope, cfg, key, lock.Name(), set))
	t.Logf("released journal reads in set checkpoint: %d", reads)
	// Admission authenticates the released history once. Its write-boundary
	// rechecks remain; nested retirement must not reconstruct that history.
	if reads > 8 {
		t.Fatalf("repeated released history admission: %d reads", reads)
	}
	var next receiveInventoryCheckpoint
	raw, err := os.ReadFile(lock.Name() + ".inventory.json")
	must(err)
	must(json.Unmarshal(raw, &next))
	proof, err := admitReleasedReceiveCheckpoints(ctx, api, store, scope, cfg, key, lock.Name(), next, set.Metadata)
	must(err)
	var member admittedReceiveRetirement
	for _, value := range set.Retirements {
		member = value
	}
	memberScope := scope
	memberScope.MachineUID = member.Withdrawal.MachineUID
	t.Run("nested-native-checkpoint", func(t *testing.T) {
		candidate := member
		candidate.releasedCheckpoint = &proof
		reads = 0
		memberReader := reader
		memberReader.Name, memberReader.ExpectedUID = retiring.Name, retiring.ExpectedUID
		must(checkpointReceiveRetirementLocked(ctx, api, memberReader, memberScope, cfg, key, lock.Name(), candidate))
		t.Logf("released journal reads in nested checkpoint: %d", reads)
		if reads > 3 {
			t.Fatalf("nested checkpoint reconstructed released history: %d reads", reads)
		}
	})
	t.Run("execution-shares-with-native-backend", func(t *testing.T) {
		pod, err := api.CoreV1().Pods(scope.Namespace).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "shared-checkpoint-agent"},
			Spec:       corev1.PodSpec{NodeName: cfg.nodeName, Containers: []corev1.Container{{Name: "dialer", Image: "test.invalid/agent"}}},
		}, metav1.CreateOptions{})
		must(err)
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "dialer", Ready: true, ContainerID: "containerd://shared", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}}}}
		pod, err = api.CoreV1().Pods(scope.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
		must(err)
		callCfg := cfg
		callCfg.receiptPodName, callCfg.receiptPodUID = pod.Name, string(pod.UID)
		must(lock.Close())
		defer func() {
			lock, err = lockReconciliation(cfg)
			must(err)
		}()
		called := false
		_, err = executeReceiveRetirementCohort(ctx, api, retiring, memberScope, callCfg, key, "containerd://shared", func(run context.Context, path string, admitted admittedReceiveRetirement) error {
			called = true
			if admitted.releasedCheckpoint == nil {
				return fmt.Errorf("execution did not share released admission with native backend")
			}
			nested := reader
			nested.Name, nested.ExpectedUID = retiring.Name, retiring.ExpectedUID
			reads = 0
			if err := checkpointReceiveRetirementLocked(run, api, nested, memberScope, callCfg, key, path, admitted); err != nil {
				return err
			}
			if reads > 3 {
				return fmt.Errorf("native backend reconstructed released history: %d reads", reads)
			}
			return nil
		})
		must(err)
		if !called {
			t.Fatal("native backend was not exercised")
		}
	})
	for _, fault := range []string{"empty", "binding", "membership", "cancelled", "released-source"} {
		t.Run(fault, func(t *testing.T) {
			candidate := proof
			callCtx := ctx
			switch fault {
			case "empty":
				candidate = receiveReleasedCheckpointAdmission{}
			case "binding":
				candidate.nextSHA = "wrong"
			case "membership":
				candidate.records = nil
			case "cancelled":
				var cancel context.CancelFunc
				callCtx, cancel = context.WithCancel(ctx)
				cancel()
			}
			before, err := os.ReadFile(lock.Name() + ".inventory.json")
			must(err)
			call := func() error {
				inherited := member
				inherited.releasedCheckpoint = &candidate
				return checkpointReceiveRetirementLocked(callCtx, api, retiring, memberScope, cfg, key, lock.Name(), inherited)
			}
			if fault == "released-source" {
				verifyReleasedJournalExecutionGuard(t, releasedStore, call)
			} else if call() == nil {
				t.Fatal("invalid shared release admission accepted")
			}
			after, err := os.ReadFile(lock.Name() + ".inventory.json")
			must(err)
			if string(before) != string(after) {
				t.Fatal("failed admission rewrote durable inventory")
			}
		})
	}
}
