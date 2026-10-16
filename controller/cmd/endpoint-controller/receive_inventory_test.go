package main

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

type inventoryCheckedCreates struct {
	typedcore.ConfigMapInterface
	check func(context.Context, *corev1.ConfigMap) error
}

func (c inventoryCheckedCreates) Create(ctx context.Context, object *corev1.ConfigMap, opts metav1.CreateOptions) (*corev1.ConfigMap, error) {
	if err := c.check(ctx, object); err != nil {
		return nil, err
	}
	return c.ConfigMapInterface.Create(ctx, object, opts)
}

type inventoryRacingUpdate struct {
	typedcore.ConfigMapInterface
	before func() error
}

func (c inventoryRacingUpdate) Update(ctx context.Context, object *corev1.ConfigMap, opts metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	if err := c.before(); err != nil {
		return nil, err
	}
	return c.ConfigMapInterface.Update(ctx, object, opts)
}

// Reuse the real API server that exercises normal journal reconciliation.
func verifyReceiveInventory(t *testing.T, r *meshReconciler, machineUID string) {
	t.Helper()
	ctx := context.Background()
	mesh := &corev1.Secret{}
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
		t.Fatal(err)
	}
	maps := r.receiveJournal.ConfigMaps
	cluster := r.receiveJournal.ClusterUID
	load := func() (tunnel.ReceiveInventory, *corev1.ConfigMap) {
		t.Helper()
		v, o, err := tunnel.ReadReceiveInventory(ctx, maps, cluster, mesh.Namespace, mesh.Name, string(mesh.UID))
		if err != nil {
			t.Fatal(err)
		}
		return v, o
	}
	inventory, original := load()
	if !slices.Equal(inventory.MachineUIDs, []string{machineUID}) {
		t.Fatal("normal reconciliation omitted journal reservation", inventory.MachineUIDs)
	}
	reserve := func(uid string) error { return tunnel.ReserveReceiveJournal(ctx, maps, cluster, mesh, uid, r.owners()) }
	if err := reserve(machineUID); err != nil {
		t.Fatal(err)
	}
	_, unchanged := load()
	if unchanged.ResourceVersion != original.ResourceVersion {
		t.Fatal("idempotent reservation rewrote index")
	}
	racing := inventoryRacingUpdate{ConfigMapInterface: maps, before: func() error { return reserve("concurrent-worker") }}
	if err := tunnel.ReserveReceiveJournal(ctx, racing, cluster, mesh, "second-worker", r.owners()); !apierrors.IsConflict(err) {
		t.Fatalf("reservation overwrote concurrent entry: %v", err)
	}
	inventory, _ = load()
	if !slices.Contains(inventory.MachineUIDs, "concurrent-worker") || slices.Contains(inventory.MachineUIDs, "second-worker") {
		t.Fatal("conflicting reservation lost atomicity")
	}
	if err := reserve("second-worker"); err != nil {
		t.Fatal(err)
	}
	inventory, valid := load()
	if len(inventory.MachineUIDs) != 3 {
		t.Fatal("retry lost another worker")
	}
	for _, uid := range []string{"concurrent-worker", "second-worker"} {
		if _, err := maps.Get(ctx, tunnel.ReceiveTransitionName(uid), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatal("reservation unexpectedly created a journal", err)
		}
	}
	if _, _, err := tunnel.ReadReceiveInventory(ctx, maps, "foreign-cluster", mesh.Namespace, mesh.Name, string(mesh.UID)); err == nil {
		t.Fatal("accepted foreign cluster inventory")
	}
	if _, _, err := tunnel.ReadReceiveInventory(ctx, maps, cluster, mesh.Namespace, "foreign-mesh", string(mesh.UID)); err == nil {
		t.Fatal("accepted renamed mesh")
	}
	for _, raw := range []string{"{", `{"version":999}`, valid.Data["inventory.json"] + ` {}`} {
		_, current := load()
		current.Data["inventory.json"] = raw
		damaged, err := maps.Update(ctx, current, metav1.UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := reserve("new-worker"); err == nil {
			t.Fatal("corrupt inventory replaced by ordinary reservation")
		}
		after, err := maps.Get(ctx, current.Name, metav1.GetOptions{})
		if err != nil || after.ResourceVersion != damaged.ResourceVersion {
			t.Fatal("rejected reservation rewrote inventory", err)
		}
		after.Data = valid.DeepCopy().Data
		if _, err := maps.Update(ctx, after, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// A lost read or write cannot be interpreted as successful discovery.
	failing := inventoryRacingUpdate{ConfigMapInterface: maps, before: func() error { return errors.New("storage unavailable") }}
	if err := tunnel.ReserveReceiveJournal(ctx, failing, cluster, mesh, "uncommitted-worker", r.owners()); err == nil {
		t.Fatal("failed reservation accepted")
	}
	inventory, _ = load()
	if slices.Contains(inventory.MachineUIDs, "uncommitted-worker") {
		t.Fatal("failed reservation persisted")
	}
	_, current := load()
	if err := maps.Delete(ctx, current.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reserve(machineUID); err == nil {
		t.Fatal("deleted inventory was recreated despite an existing journal")
	}
	if _, err := maps.Get(ctx, current.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("failed recovery recreated inventory", err)
	}
	// Restore the fixture explicitly for subsequent journal phase tests.
	current.UID, current.ResourceVersion = "", ""
	if _, err := maps.Create(ctx, current, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}
