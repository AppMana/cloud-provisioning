package attachment

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type retirementInventoryWriter struct {
	typedcore.ConfigMapInterface
	before       func() error
	lostResponse bool
}

func (w retirementInventoryWriter) Update(ctx context.Context, object *corev1.ConfigMap, opts metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	if w.before != nil {
		if err := w.before(); err != nil {
			return nil, err
		}
	}
	stored, err := w.ConfigMapInterface.Update(ctx, object, opts)
	if err == nil && w.lostResponse {
		return nil, errors.New("injected lost write response")
	}
	return stored, err
}

// The API and etcd are isolated. Terminal phase receipts are synthetic; this
// verifies persistence and concurrency, not native retirement or continuity.
func TestReceiveWithdrawalPersistenceWithRealAPI(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for isolated API validation")
	}
	env := &envtest.Environment{}
	config, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	api, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	verifyExpiredReceiveRemoval(t, api)
	verifySiblingReceiveRemoval(t, api)
	ctx := context.Background()
	if _, err := api.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	mesh, err := api.CoreV1().Secrets("test").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w, fixture, inventory, _ := retirementFixture(t, "forward", true, string(mesh.UID))
	mesh.Data = map[string][]byte{tunnel.NodePublicKeyPrefix + "site": []byte(w.Consumers[0].PublicKey), tunnel.PeerPublicKeyPrefix + w.MachineName: []byte(w.PublicKey)}
	mesh, err = api.CoreV1().Secrets("test").Update(ctx, mesh, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w.SourceVersion = mesh.ResourceVersion
	raw, err := fixture.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint struct {
		Object corev1.ConfigMap `json:"object"`
	}
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal(err)
	}
	checkpoint.Object.UID, checkpoint.Object.ResourceVersion = "", ""
	maps := api.CoreV1().ConfigMaps("test")
	journal, err := maps.Create(ctx, &checkpoint.Object, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	store := handover.Store{ConfigMaps: maps, ClusterUID: "cluster", Name: journal.Name, ExpectedUID: string(journal.UID)}
	snapshot, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inventory.Publications[w.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(journal.UID)}
	raw, _ = json.Marshal(inventory)
	index, err := maps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveInventoryName(w.MeshUID)}, Data: map[string]string{"inventory.json": string(raw)}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	read := func() tunnel.ReceiveInventory {
		t.Helper()
		v, _, err := tunnel.ReadReceiveInventory(ctx, maps, "cluster", "test", mesh.Name, string(mesh.UID))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	reserve := func(storage typedcore.ConfigMapInterface) error {
		return w.ReserveReceiveWithdrawal(ctx, storage, "cluster", string(index.UID), mesh, snapshot)
	}
	race := retirementInventoryWriter{ConfigMapInterface: maps, before: func() error {
		return tunnel.ReserveReceiveJournal(ctx, maps, "cluster", mesh, "other-machine", nil)
	}}
	if err := reserve(race); !apierrors.IsConflict(err) {
		t.Fatalf("concurrent reservation was not protected: %v", err)
	}
	if got := read(); len(got.MachineUIDs) != 2 || len(got.Publications[w.MachineUID].Retirement) != 0 {
		t.Fatal("conflict changed withdrawal or lost another worker")
	}
	if err := reserve(retirementInventoryWriter{ConfigMapInterface: maps, lostResponse: true}); err == nil {
		t.Fatal("lost response accepted")
	}
	if len(read().Publications[w.MachineUID].Retirement) == 0 {
		t.Fatal("write was not committed before response loss")
	}
	if err := reserve(maps); err != nil {
		t.Fatal("identical retry failed", err)
	}
	if _, err := ReadReceiveWithdrawal(read(), w.MachineUID, snapshot, mesh); err == nil {
		t.Fatal("stored intent admitted before peer withdrawal")
	}
	// A lost persistence response can resume publication from the exact retained
	// recipients; it must not use the post-publication native admission path.
	prepared, err := ReadPreparedReceiveWithdrawal(read(), w.MachineUID, snapshot, mesh)
	if err != nil || !reflect.DeepEqual(prepared, w) {
		t.Fatal("persisted preparation could not resume", err)
	}
	for _, fault := range []string{"revision", "replacement-peer", "absent-peer", "changed-history", "missing-recipient", "trailing"} {
		t.Run("prepared-retry/"+fault, func(t *testing.T) {
			v := read()
			source := mesh.DeepCopy()
			switch fault {
			case "revision":
				source.ResourceVersion = "different"
			case "replacement-peer":
				source.Data[tunnel.PeerPublicKeyPrefix+w.MachineName] = []byte("replacement")
			case "absent-peer":
				delete(source.Data, tunnel.PeerPublicKeyPrefix+w.MachineName)
			default:
				p := v.Publications[w.MachineUID]
				var record receiveWithdrawalRecord
				if err := json.Unmarshal(p.Retirement, &record); err != nil {
					t.Fatal(err)
				}
				switch fault {
				case "changed-history":
					record.JournalSHA256 = "different"
				case "missing-recipient":
					record.Withdrawal.Consumers = nil
				}
				p.Retirement, _ = json.Marshal(record)
				if fault == "trailing" {
					p.Retirement = append(p.Retirement, []byte(" {}")...)
				}
				v.Publications[w.MachineUID] = p
			}
			if _, err := ReadPreparedReceiveWithdrawal(v, w.MachineUID, snapshot, source); err == nil {
				t.Fatal("changed preparation admitted")
			}
		})
	}
	changed := w
	changed.Consumers = nil
	if err := changed.ReserveReceiveWithdrawal(ctx, maps, "cluster", string(index.UID), mesh, snapshot); err == nil {
		t.Fatal("missing participant replaced intent")
	}
	changed = w
	changed.DrainIntent = "different/action"
	if err := changed.ReserveReceiveWithdrawal(ctx, maps, "cluster", string(index.UID), mesh, snapshot); err == nil {
		t.Fatal("different action replaced intent")
	}
	if err := w.ReserveReceiveWithdrawal(ctx, maps, "cluster", "recreated-index", mesh, snapshot); err == nil {
		t.Fatal("wrong index incarnation accepted")
	}
	mesh.Data, _, err = tunnel.WithdrawRemotePeer(mesh.Data, w.MachineName, w.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	mesh, err = api.CoreV1().Secrets("test").Update(ctx, mesh, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPreparedReceiveWithdrawal(read(), w.MachineUID, snapshot, mesh); err == nil {
		t.Fatal("published withdrawal admitted as unpublished preparation")
	}
	retained := read()
	got, err := ReadReceiveWithdrawal(retained, w.MachineUID, snapshot, mesh)
	if err != nil || !reflect.DeepEqual(got, w) {
		t.Fatal("withdrawal lost durable participants", err)
	}
	receiptScope, err := NewReceiveRetirementReceiptScope(ctx, maps, "cluster", string(index.UID), snapshot, mesh)
	if err != nil || len(receiptScope.Digest()) != 64 || len(receiptScope.nodes) != len(w.Consumers) {
		t.Fatal("missing exact withdrawal receipt scope", err)
	}
	repeated, err := NewReceiveRetirementReceiptScope(ctx, maps, "cluster", string(index.UID), snapshot, mesh)
	if err != nil || repeated.Digest() != receiptScope.Digest() {
		t.Fatal("unchanged withdrawal changed receipt binding", err)
	}
	if _, err := NewReceiveRetirementReceiptScope(ctx, maps, "cluster", "replacement-index", snapshot, mesh); err == nil {
		t.Fatal("replacement index inherited receipt scope")
	}
	changedMesh := mesh.DeepCopy()
	changedMesh.ResourceVersion += "-changed"
	changedScope, err := NewReceiveRetirementReceiptScope(ctx, maps, "cluster", string(index.UID), snapshot, changedMesh)
	if err != nil || changedScope.Digest() == receiptScope.Digest() {
		t.Fatal("mesh revision not bound to retirement observation", err)
	}
	revisedIndex, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	revisedIndex.Annotations = map[string]string{"receipt-revision": "changed"}
	if _, err = maps.Update(ctx, revisedIndex, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	revisedScope, err := NewReceiveRetirementReceiptScope(ctx, maps, "cluster", string(index.UID), snapshot, mesh)
	if err != nil || revisedScope.Digest() == receiptScope.Digest() {
		t.Fatal("inventory revision not bound to retirement observation", err)
	}
	// No consumer Nodes exist in this API server. Their required identities
	// remain in the intent; this read must not manufacture retirement receipts.
	if len(got.Consumers) != 1 || len(retained.MachineUIDs) != 2 {
		t.Fatal("absence released a reservation or participant")
	}
	for _, fault := range []string{"hash", "unknown", "trailing", "version"} {
		v := read()
		p := v.Publications[w.MachineUID]
		var record map[string]any
		if err := json.Unmarshal(p.Retirement, &record); err != nil {
			t.Fatal(err)
		}
		switch fault {
		case "hash":
			record["journalSHA256"] = "changed"
		case "unknown":
			record["unrecognized"] = true
		case "version":
			record["version"] = 99
		}
		p.Retirement, _ = json.Marshal(record)
		if fault == "trailing" {
			p.Retirement = append(p.Retirement, []byte(" {}")...)
		}
		v.Publications[w.MachineUID] = p
		if _, err := ReadReceiveWithdrawal(v, w.MachineUID, snapshot, mesh); err == nil {
			t.Fatalf("accepted %s record", fault)
		}
	}
	// Storage-only completion fixture: synthetic evidence exercises CAS and
	// retention, never authentication or native completion authority.
	t.Run("completion-storage", func(t *testing.T) {
		evidence := json.RawMessage(`{"version":1,"fixture":"synthetic-completion"}`)
		before := read()
		meta, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		intent := before.Publications[w.MachineUID].Retirement
		persist := func(storage typedcore.ConfigMapInterface, expected metav1.ObjectMeta, record json.RawMessage) error {
			return tunnel.RecordReceiveRetirementCompletion(ctx, storage, "cluster", expected, mesh, snapshot, intent, record)
		}
		for _, bad := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`{} {}`)} {
			if err := persist(maps, meta.ObjectMeta, bad); err == nil {
				t.Fatal("invalid completion record accepted")
			}
		}
		if err := tunnel.RecordReceiveRetirementCompletion(ctx, maps, "cluster", meta.ObjectMeta, mesh, snapshot, json.RawMessage(`{}`), evidence); err == nil {
			t.Fatal("changed withdrawal accepted")
		}
		racing := retirementInventoryWriter{ConfigMapInterface: maps, before: func() error {
			latest, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			latest.Annotations = map[string]string{"completion-race": "changed"}
			_, err = maps.Update(ctx, latest, metav1.UpdateOptions{})
			return err
		}}
		if err := persist(racing, meta.ObjectMeta, evidence); !apierrors.IsConflict(err) {
			t.Fatal("completion conflict was not returned", err)
		}
		if len(read().Publications[w.MachineUID].RetirementCompletion) != 0 {
			t.Fatal("conflicting completion persisted")
		}
		if err := persist(maps, meta.ObjectMeta, evidence); err == nil {
			t.Fatal("stale authentication revision accepted")
		}
		meta, err = maps.Get(ctx, index.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		lost := retirementInventoryWriter{ConfigMapInterface: maps, lostResponse: true}
		if err := persist(lost, meta.ObjectMeta, evidence); err == nil {
			t.Fatal("lost response concealed")
		}
		after := read()
		if string(after.Publications[w.MachineUID].RetirementCompletion) != string(evidence) {
			t.Fatal("ambiguous write evidence not retained")
		}
		item := after.Publications[w.MachineUID]
		item.RetirementCompletion = nil
		after.Publications[w.MachineUID] = item
		if !reflect.DeepEqual(before, after) {
			t.Fatal("completion changed reservations or another worker")
		}
		if err := persist(maps, meta.ObjectMeta, evidence); err == nil {
			t.Fatal("ambiguous write blindly retried")
		}
		meta, err = maps.Get(ctx, index.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := persist(maps, meta.ObjectMeta, json.RawMessage(`{"changed":true}`)); err == nil {
			t.Fatal("completion replaced")
		}
		if err := persist(maps, meta.ObjectMeta, evidence); err != nil {
			t.Fatal("reloaded identical completion failed", err)
		}
		meta, err = maps.Get(ctx, index.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		fresh := json.RawMessage(`{"version":2,"fixture":"fresh-completion"}`)
		refresh := func(storage typedcore.ConfigMapInterface, previous json.RawMessage) error {
			return tunnel.RefreshReceiveRetirementCompletion(ctx, storage, "cluster", meta.ObjectMeta, mesh, snapshot, intent, previous, fresh)
		}
		for _, wrong := range []json.RawMessage{nil, json.RawMessage(`{}`)} {
			if err := refresh(maps, wrong); err == nil {
				t.Fatal("refresh ignored previous evidence")
			}
		}
		refreshRace := retirementInventoryWriter{ConfigMapInterface: maps, before: func() error {
			latest, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			latest.Annotations = map[string]string{"refresh-race": "changed"}
			_, err = maps.Update(ctx, latest, metav1.UpdateOptions{})
			return err
		}}
		if err := refresh(refreshRace, evidence); !apierrors.IsConflict(err) {
			t.Fatal("refresh ignored concurrent writer", err)
		}
		if string(read().Publications[w.MachineUID].RetirementCompletion) != string(evidence) {
			t.Fatal("conflicting refresh lost old evidence")
		}
		meta, err = maps.Get(ctx, index.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := refresh(lost, evidence); err == nil {
			t.Fatal("lost refresh response concealed")
		}
		refreshed := read()
		if string(refreshed.Publications[w.MachineUID].RetirementCompletion) != string(fresh) {
			t.Fatal("refresh was not persisted")
		}
		if err := refresh(maps, evidence); err == nil {
			t.Fatal("stale refresh overwrote new evidence")
		}
		p := refreshed.Publications[w.MachineUID]
		p.RetirementCompletion = nil
		refreshed.Publications[w.MachineUID] = p
		if !reflect.DeepEqual(before, refreshed) {
			t.Fatal("refresh changed reserved ownership")
		}
	})
	t.Run("finalization-storage", func(t *testing.T) {
		verifyRetirementFinalizationStorage(t, maps, mesh, snapshot, index.Name, w.MachineUID)
	})
	t.Run("release-storage", func(t *testing.T) {
		verifyReceiveReleaseStorage(t, maps, mesh, snapshot, w.MachineUID)
	})
	// An index incarnation is not transferable, even with identical JSON.
	current, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	uid := types.UID(index.UID)
	if err := maps.Delete(ctx, index.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
		t.Fatal(err)
	}
	current.UID, current.ResourceVersion = "", ""
	if _, err := maps.Create(ctx, current, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := tunnel.ReserveReceiveRetirement(ctx, maps, "cluster", string(index.UID), mesh, snapshot, retained.Publications[w.MachineUID].Retirement); err == nil {
		t.Fatal("recreated inventory accepted")
	}
}
