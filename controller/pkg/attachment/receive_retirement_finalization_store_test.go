package attachment

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Real API storage with synthetic completion/finalization records. This proves
// CAS, immutable evidence and reservation retention, not authentication, native
// cleanup, finalization semantics, reader rollout or allocation release.
func verifyRetirementFinalizationStorage(t *testing.T, maps typedcore.ConfigMapInterface, mesh *corev1.Secret, snapshot handover.Snapshot, inventoryName, machineUID string) {
	t.Helper()
	ctx := context.Background()
	read := func() (tunnel.ReceiveInventory, *corev1.ConfigMap) {
		v, obj, err := tunnel.ReadReceiveInventory(ctx, maps, "cluster", mesh.Namespace, mesh.Name, string(mesh.UID))
		if err != nil {
			t.Fatal(err)
		}
		return v, obj
	}
	before, object := read()
	publication := before.Publications[machineUID]
	if len(publication.RetirementCompletion) == 0 {
		t.Fatal("fixture has no completion")
	}
	record := json.RawMessage(`{"version":1,"fixture":"synthetic-finalization"}`)
	persist := func(api typedcore.ConfigMapInterface, meta metav1.ObjectMeta, consumed, final json.RawMessage, check func() error) error {
		return tunnel.RecordReceiveRetirementFinalization(ctx, api, "cluster", meta, mesh, snapshot, publication.Retirement, consumed, final, check)
	}
	check := func() error { return nil }
	for _, bad := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`{} {}`)} {
		if err := persist(maps, object.ObjectMeta, publication.RetirementCompletion, bad, check); err == nil {
			t.Fatal("invalid finalization accepted")
		}
	}
	for _, bad := range []json.RawMessage{nil, json.RawMessage(`{}`)} {
		if err := persist(maps, object.ObjectMeta, bad, record, check); err == nil {
			t.Fatal("changed or missing consumed completion accepted")
		}
	}
	if err := persist(maps, object.ObjectMeta, publication.RetirementCompletion, record, nil); err == nil {
		t.Fatal("missing final write check accepted")
	}
	denied := errors.New("freshness or owner check failed")
	if err := persist(maps, object.ObjectMeta, publication.RetirementCompletion, record, func() error { return denied }); !errors.Is(err, denied) {
		t.Fatal("final write check ignored", err)
	}
	race := func() error {
		current, err := maps.Get(ctx, inventoryName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		current.Annotations = map[string]string{"finalization-test": "concurrent-writer"}
		_, err = maps.Update(ctx, current, metav1.UpdateOptions{})
		return err
	}
	if err := persist(maps, object.ObjectMeta, publication.RetirementCompletion, record, race); !apierrors.IsConflict(err) {
		t.Fatal("finalization ignored concurrent inventory update", err)
	}
	current, _ := read()
	if len(current.Publications[machineUID].RetirementFinalization) != 0 {
		t.Fatal("failed write finalized retirement")
	}
	if err := persist(maps, object.ObjectMeta, publication.RetirementCompletion, record, check); err == nil {
		t.Fatal("stale verified revision reused")
	}
	_, object = read()
	cancelled, cancel := context.WithCancel(ctx)
	err := tunnel.RecordReceiveRetirementFinalization(cancelled, maps, "cluster", object.ObjectMeta, mesh, snapshot, publication.Retirement, publication.RetirementCompletion, record, func() error { cancel(); return nil })
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled finalization accepted", err)
	}
	lost := retirementInventoryWriter{ConfigMapInterface: maps, lostResponse: true}
	if err := persist(lost, object.ObjectMeta, publication.RetirementCompletion, record, check); err == nil {
		t.Fatal("ambiguous response concealed")
	}
	after, _ := read()
	finalized := after.Publications[machineUID]
	if string(finalized.RetirementFinalization) != string(record) {
		t.Fatal("finalization not persisted")
	}
	finalized.RetirementFinalization = nil
	after.Publications[machineUID] = finalized
	if !reflect.DeepEqual(before, after) {
		t.Fatal("finalization changed consumed evidence or reservations")
	}
	if err := persist(maps, object.ObjectMeta, publication.RetirementCompletion, record, check); err == nil {
		t.Fatal("ambiguous finalization blindly retried")
	}
	_, object = read()
	if err := persist(maps, object.ObjectMeta, publication.RetirementCompletion, json.RawMessage(`{"changed":true}`), check); err == nil {
		t.Fatal("finalization replaced")
	}
	if err := tunnel.RefreshReceiveRetirementCompletion(ctx, maps, "cluster", object.ObjectMeta, mesh, snapshot, publication.Retirement, publication.RetirementCompletion, json.RawMessage(`{"changed":true}`)); err == nil {
		t.Fatal("consumed completion replaced")
	}
	calls := 0
	if err := persist(maps, object.ObjectMeta, publication.RetirementCompletion, record, func() error { calls++; return nil }); err != nil {
		t.Fatal("freshly admitted identical retry rejected", err)
	}
	if calls != 1 {
		t.Fatal("retry bypassed final write check")
	}
	if err := tunnel.ReserveReceiveJournal(ctx, maps, "cluster", mesh, machineUID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tunnel.PublishReceiveJournal(ctx, maps, "cluster", mesh, snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tunnel.ReserveReceiveRetirement(ctx, maps, "cluster", string(object.UID), mesh, snapshot, publication.Retirement); err != nil {
		t.Fatal(err)
	}
	preserved, _ := read()
	if string(preserved.Publications[machineUID].RetirementFinalization) != string(record) {
		t.Fatal("ordinary reservation, publication or withdrawal erased finalization")
	}
}
