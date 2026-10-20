package main

import (
	"context"
	"encoding/json"
	"fmt"
	"k8s.io/client-go/kubernetes"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Journal/index persistence uses the existing API server; owner absence and
// survivors use synthetic objects. This does not measure native retirement,
// authenticated completion, release, or live network continuity.
func verifyOrphanReceiveReconciliation(t *testing.T, r *meshReconciler, store handover.Store, snapshot handover.Snapshot, normalMode ...bool) {
	t.Helper()
	ctx := context.Background()
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	if err != nil {
		t.Fatal(err)
	}
	mesh := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
		t.Fatal(err)
	}
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Freezing sets Kubernetes immutable=true and cannot be undone. Use
	// separate API storage for the destructive journal lifecycle, preserving the
	// preparation fixture's journal incarnation for its later successor tests.
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	}()
	isolatedAPI, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := isolatedAPI.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: mesh.Namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	store.ConfigMaps = isolatedAPI.CoreV1().ConfigMaps(mesh.Namespace)
	store.ExpectedUID = ""
	journal = journal.DeepCopy()
	journal.UID = ""
	journal.ResourceVersion = ""
	journal.Immutable = nil
	journal, err = store.ConfigMaps.Create(ctx, journal, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pub := inventory.Publications[plan.MachineUID]
	pub.JournalUID = string(journal.UID)
	inventory.Publications[plan.MachineUID] = pub
	rawIndex, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	index = index.DeepCopy()
	index.UID = ""
	index.ResourceVersion = ""
	index.Data["inventory.json"] = string(rawIndex)
	index, err = store.ConfigMaps.Create(ctx, index, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	published := mesh.DeepCopy()
	mesh.Data, _, err = tunnel.WithdrawRemotePeer(mesh.Data, plan.MachineName, plan.RootPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{mesh}
	nodes := &corev1.NodeList{}
	if err := r.reader.List(ctx, nodes); err != nil {
		t.Fatal(err)
	}
	for i := range nodes.Items {
		if nodes.Items[i].Name != plan.CloudNodeName {
			objects = append(objects, nodes.Items[i].DeepCopy())
		}
	}
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(machineGVK.GroupVersion().WithKind("MachineList"))
	if err := r.reader.List(ctx, machines); err != nil {
		t.Fatal(err)
	}
	for i := range machines.Items {
		if machines.Items[i].GetName() != plan.MachineName {
			objects = append(objects, machines.Items[i].DeepCopy())
		}
	}
	secrets := &corev1.SecretList{}
	if err := r.reader.List(ctx, secrets, client.InNamespace(mesh.Namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range secrets.Items {
		if secrets.Items[i].Name != mesh.Name && secrets.Items[i].Name != plan.AdoptionName {
			objects = append(objects, secrets.Items[i].DeepCopy())
		}
	}
	api := fake.NewClientBuilder().WithScheme(r.Scheme()).WithObjects(objects...).Build()
	if len(normalMode) != 0 && normalMode[0] {
		// Model the publication update's new revision; normal withdrawal must
		// not admit the same mesh revision captured before peer removal.
		if err := api.Update(ctx, mesh); err != nil {
			t.Fatal(err)
		}
	}
	test := *r
	test.receiveJournal = store
	test.Client = api
	test.reader = api
	policy := *r.receivePreparation
	jp := *policy.Journal
	jp.Receipts = &receiveReceiptPolicy{}
	policy.Journal = &jp
	test.receivePreparation = &policy
	// The shared fixture also contains deliberately unpublished reservations.
	// They must fence orphan mutation before exercising a fully published set.
	if progress, err := test.reconcileOrphanReceiveWithdrawals(ctx, mesh); err != nil || progress {
		t.Fatal("unfinished inventory did not fence orphan recovery", err)
	}
	inventory.MachineUIDs = []string{plan.MachineUID}
	inventory.Publications = map[string]tunnel.ReceiveJournalPublication{plan.MachineUID: inventory.Publications[plan.MachineUID]}
	raw, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	isolated := index.DeepCopy()
	isolated.Data["inventory.json"] = string(raw)
	if _, err := store.ConfigMaps.Update(ctx, isolated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(normalMode) != 0 && normalMode[0] {
		t.Run("adoption-delete-preconditions", func(t *testing.T) {
			realClient, err := client.New(cfg, client.Options{})
			if err != nil {
				t.Fatal(err)
			}
			verifyRetiredAdoptionDeletePreconditions(t, realClient, mesh.Namespace)
		})
		snapshot, err := store.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err = store.FreezeCompleted(ctx, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		resolver := attachment.MeshConsumerResolver{Reader: api, Namespace: mesh.Namespace, SecretName: mesh.Name, SecretUID: string(mesh.UID)}
		consumers, err := resolver.PublishedConsumers(ctx, mesh)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(published.Data)
		if err != nil {
			t.Fatal(err)
		}
		intent := attachment.RemoteWithdrawalIntent{MeshNamespace: mesh.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), SourceVersion: published.ResourceVersion, SourceDataHash: tunnel.HashPeerList(raw), MachineName: plan.MachineName, MachineUID: plan.MachineUID, PublicKey: plan.RootPublicKey, DrainIntent: "test/normal-removal", Consumers: consumers}
		if err := intent.ReserveReceiveWithdrawal(ctx, store.ConfigMaps, store.ClusterUID, string(index.UID), published, snapshot); err != nil {
			t.Fatal(err)
		}
		verifyNormalRetirementControllerLifecycle(t, &test, api, mesh, store, plan)
		return
	}
	for pass := 0; pass < 2; pass++ {
		progress, err := test.reconcileOrphanReceiveWithdrawals(ctx, mesh)
		if err != nil || !progress {
			t.Fatalf("orphan stage %d: progress=%v err=%v", pass, progress, err)
		}
		current, currentIndex, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(current.MachineUIDs, inventory.MachineUIDs) || currentIndex.UID != index.UID {
			t.Fatal("orphan recovery released or replaced inventory")
		}
		if pass == 0 && len(current.Publications[plan.MachineUID].Retirement) != 0 {
			t.Fatal("freeze also published intent")
		}
	}
	current, currentIndex, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := attachment.ReadReceiveWithdrawal(current, plan.MachineUID, frozen, mesh)
	if err != nil || !intent.Orphaned {
		t.Fatal("persisted orphan intent not admitted", err)
	}
	progress, err := test.reconcileOrphanReceiveWithdrawals(ctx, mesh)
	if err != nil || progress {
		t.Fatal("retry recaptured orphan intent", err)
	}
	_, last, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil || last.ResourceVersion != currentIndex.ResourceVersion {
		t.Fatal("retry rewrote persisted consumers", err)
	}
	for _, released := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty-active-released-%v", released), func(t *testing.T) {
			empty := current
			empty.MachineUIDs = []string{}
			empty.Publications = nil
			if released {
				p := current.Publications[plan.MachineUID]
				p.RetirementCompletion, p.RetirementFinalization = json.RawMessage(`{}`), json.RawMessage(`{}`)
				empty.Released = map[string]tunnel.ReceiveJournalPublication{plan.MachineUID: p}
			}
			raw, err := json.Marshal(empty)
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			before.Data["inventory.json"] = string(raw)
			before, err = store.ConfigMaps.Update(ctx, before, metav1.UpdateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if progress, err := test.reconcileOrphanReceiveWithdrawals(ctx, mesh); err != nil || progress {
				t.Fatal("empty active set blocked ordinary reconciliation", err)
			}
			after, err := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
			if err != nil || before.ResourceVersion != after.ResourceVersion {
				t.Fatal("empty reconciliation mutated retained state", err)
			}
		})
	}

}
