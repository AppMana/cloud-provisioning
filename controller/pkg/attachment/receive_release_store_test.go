package attachment

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Actual inventory CAS with synthetic finalization and release admission. No
// consumer fence authentication, native absence, address reuse, reboot or live
// release is qualified. The retained frozen journal must survive every case.
func verifyReceiveReleaseStorage(t *testing.T, maps typedcore.ConfigMapInterface, mesh *corev1.Secret, snapshot handover.Snapshot, machineUID string) {
	t.Helper()
	ctx := context.Background()
	read := func() (tunnel.ReceiveInventory, *corev1.ConfigMap) {
		v, o, e := tunnel.ReadReceiveInventory(ctx, maps, "cluster", mesh.Namespace, mesh.Name, string(mesh.UID))
		if e != nil {
			t.Fatal(e)
		}
		return v, o
	}
	before, obj := read()
	consumed := before.Publications[machineUID]
	if len(consumed.RetirementFinalization) == 0 {
		t.Fatal("fixture lacks finalization")
	}
	allow := func() error { return nil }
	persist := func(api typedcore.ConfigMapInterface, meta metav1.ObjectMeta, p tunnel.ReceiveJournalPublication, check func() error) error {
		return tunnel.ReleaseReceiveJournal(ctx, api, "cluster", meta, mesh, snapshot, p, check)
	}
	for _, fault := range []string{"missing-check", "missing-finalization", "changed-completion", "changed-journal", "stale-version", "wrong-uid", "denied"} {
		p, meta, check := consumed, obj.ObjectMeta, allow
		denied := errors.New("release ownership denied")
		switch fault {
		case "missing-check":
			check = nil
		case "missing-finalization":
			p.RetirementFinalization = nil
		case "changed-completion":
			p.RetirementCompletion = json.RawMessage(`{"changed":true}`)
		case "changed-journal":
			p.JournalUID = "replacement"
		case "stale-version":
			meta.ResourceVersion = "stale"
		case "wrong-uid":
			meta.UID = "replacement"
		case "denied":
			check = func() error { return denied }
		}
		if err := persist(maps, meta, p, check); err == nil {
			t.Fatal("invalid release accepted", fault)
		}
		current, _ := read()
		if !reflect.DeepEqual(current, before) {
			t.Fatal("rejected release mutated inventory", fault)
		}
	}
	// An independent reservation racing the write must survive. No automatic CAS
	// retry can reinterpret earlier release authority against the new inventory.
	raced := retirementInventoryWriter{ConfigMapInterface: maps, before: func() error {
		return tunnel.ReserveReceiveJournal(ctx, maps, "cluster", mesh, "concurrent-release-worker", nil)
	}}
	if err := persist(raced, obj.ObjectMeta, consumed, allow); !apierrors.IsConflict(err) {
		t.Fatal("racing reservation did not conflict", err)
	}
	before, obj = read()
	if !slices.Contains(before.MachineUIDs, "concurrent-release-worker") || !slices.Contains(before.MachineUIDs, machineUID) {
		t.Fatal("race lost active reservations")
	}
	cancelled, cancel := context.WithCancel(ctx)
	err := tunnel.ReleaseReceiveJournal(cancelled, maps, "cluster", obj.ObjectMeta, mesh, snapshot, consumed, func() error { cancel(); return nil })
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled release accepted", err)
	}
	lost := retirementInventoryWriter{ConfigMapInterface: maps, lostResponse: true}
	if err := persist(lost, obj.ObjectMeta, consumed, allow); err == nil {
		t.Fatal("lost response concealed")
	}
	after, current := read()
	expected := before
	expected.MachineUIDs = slices.DeleteFunc(slices.Clone(before.MachineUIDs), func(uid string) bool { return uid == machineUID })
	expected.Publications = map[string]tunnel.ReceiveJournalPublication{}
	for uid, p := range before.Publications {
		if uid != machineUID {
			expected.Publications[uid] = p
		}
	}
	// Empty maps may decode as nil through omitempty.
	if len(expected.Publications) == 0 {
		expected.Publications = nil
	}
	expected.Released = map[string]tunnel.ReceiveJournalPublication{machineUID: consumed}
	if !reflect.DeepEqual(after, expected) {
		t.Fatal("release did not atomically preserve retired evidence and unrelated ownership")
	}
	if err := persist(maps, obj.ObjectMeta, consumed, allow); err == nil {
		t.Fatal("ambiguous response blindly retried")
	}
	calls := 0
	if err := persist(maps, current.ObjectMeta, consumed, func() error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("identical retry bypassed authority check")
	}
	_, current = read()
	changed := consumed
	changed.RetirementFinalization = json.RawMessage(`{"changed":true}`)
	if err := persist(maps, current.ObjectMeta, changed, allow); err == nil {
		t.Fatal("retired evidence replaced")
	}
	if err := tunnel.ReserveReceiveJournal(ctx, maps, "cluster", mesh, machineUID, nil); err == nil {
		t.Fatal("released owner reserved again")
	}
	if err := tunnel.ReservePreparedReceiveJournal(ctx, maps, "cluster", mesh, machineUID, nil, []byte(`{}`)); err == nil {
		t.Fatal("released owner prepared again")
	}
	if _, err := tunnel.PublishReceiveJournal(ctx, maps, "cluster", mesh, snapshot, time.Now()); err == nil {
		t.Fatal("released journal republished")
	}
	if err := tunnel.ReserveReceiveJournal(ctx, maps, "cluster", mesh, "new-release-worker", nil); err != nil {
		t.Fatal(err)
	}
	after, _ = read()
	if !reflect.DeepEqual(after.Released[machineUID], consumed) {
		t.Fatal("ordinary reservation erased retired evidence")
	}
	retained, err := maps.Get(ctx, snapshot.Metadata().Name, metav1.GetOptions{})
	if err != nil || retained.UID != snapshot.Metadata().UID || retained.Immutable == nil || !*retained.Immutable {
		t.Fatal("release deleted or replaced frozen journal", err)
	}
}
