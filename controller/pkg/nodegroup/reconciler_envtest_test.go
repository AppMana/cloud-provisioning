package nodegroup

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func verifyRealAPIReconciliation(t *testing.T, api client.Client) {
	t.Helper()
	ctx := context.Background()
	group := groupFixture()
	group.Name = "replica-loop"
	group.Namespace = "default"
	group.UID = ""
	desired := int32(3)
	group.Spec.Replicas = &desired
	if err := api.Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)}
	for i := 0; i < 12; i++ {
		// Every iteration discards all controller memory.
		r := &Reconciler{API: api}
		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.Get(ctx, request.NamespacedName, group); err != nil {
		t.Fatal(err)
	}
	if group.Status.Replicas != 3 || group.Status.PendingAction != nil || !slices.Contains(group.Finalizers, GroupFinalizer) || group.Status.ReadyReplicas != 0 {
		t.Fatal("incorrect reconciled capacity", group)
	}
	claims := &v1alpha1.ProvisionedNodeClaimList{}
	if err := api.List(ctx, claims, client.InNamespace(group.Namespace), client.MatchingLabels{GroupUIDLabel: string(group.UID)}); err != nil {
		t.Fatal(err)
	}
	if len(claims.Items) != 3 {
		t.Fatal("wrong child count", len(claims.Items))
	}
	original := map[string]string{}
	for _, c := range claims.Items {
		original[c.Name] = string(c.UID)
	}
	desired = 1
	group.Spec.Replicas = &desired
	if err := api.Update(ctx, group); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{API: api}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, request); err == nil || !strings.Contains(err.Error(), "withdrawal integration") {
		t.Fatal("unimplemented removal did not hold", err)
	}
	if err := api.Get(ctx, request.NamespacedName, group); err != nil {
		t.Fatal(err)
	}
	if group.Status.PendingAction == nil || group.Status.PendingAction.Type != DrainAction || group.Status.PendingAction.Ordinal != 2 {
		t.Fatal("incorrect serialized drain", group.Status.PendingAction)
	}
	if err := api.List(ctx, claims, client.InNamespace(group.Namespace), client.MatchingLabels{GroupUIDLabel: string(group.UID)}); err != nil {
		t.Fatal(err)
	}
	if len(claims.Items) != 3 {
		t.Fatal("removed claims before drain gates")
	}
	for _, c := range claims.Items {
		if string(c.UID) != original[c.Name] || !c.DeletionTimestamp.IsZero() {
			t.Fatal("changed live child before withdrawal", c.Name)
		}
	}
}

func verifyDeletingGroupCreation(t *testing.T, api client.Client) {
	t.Helper()
	ctx := context.Background()
	for _, created := range []bool{false, true} {
		group := groupFixture()
		group.Name = "deleting-pending-create"
		if created {
			group.Name += "-fulfilled"
		}
		group.Namespace = "default"
		group.UID = ""
		group.Finalizers = []string{GroupFinalizer}
		if err := api.Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		action, err := ProposeAction(group, nil)
		if err != nil {
			t.Fatal(err)
		}
		group, err = ReserveAction(ctx, api, group, action)
		if err != nil {
			t.Fatal(err)
		}
		key := client.ObjectKeyFromObject(group)
		var childUID string
		if created {
			child, err := ResumeCreation(ctx, api, key, group.UID, action.ID)
			if err != nil {
				t.Fatal(err)
			}
			childUID = string(child.UID)
		}
		if err := api.Delete(ctx, group); err != nil {
			t.Fatal(err)
		}
		r := &Reconciler{API: api}
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		if created && err != nil {
			t.Fatal("existing child stuck during deletion", err)
		}
		if !created && err == nil {
			t.Fatal("unfulfilled reservation silently discarded")
		}
		if err := api.Get(ctx, key, group); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(group.Finalizers, GroupFinalizer) {
			t.Fatal("released deletion before lifecycle gates")
		}
		claims := &v1alpha1.ProvisionedNodeClaimList{}
		if err := api.List(ctx, claims, client.InNamespace(group.Namespace), client.MatchingLabels{GroupUIDLabel: string(group.UID)}); err != nil {
			t.Fatal(err)
		}
		if !created {
			if len(claims.Items) != 0 || group.Status.PendingAction == nil {
				t.Fatal("created capacity during group deletion")
			}
			continue
		}
		if len(claims.Items) != 1 || string(claims.Items[0].UID) != childUID || group.Status.PendingAction != nil {
			t.Fatal("lost completed child during deletion")
		}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if err := api.Get(ctx, key, group); err != nil {
			t.Fatal(err)
		}
		if group.Status.PendingAction == nil || group.Status.PendingAction.Type != DrainAction || group.Status.PendingAction.ChildUID != childUID {
			t.Fatal("deletion did not reserve existing child for drain")
		}
	}
}
