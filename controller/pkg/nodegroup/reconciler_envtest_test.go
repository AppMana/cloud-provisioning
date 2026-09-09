package nodegroup

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	"slices"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	claimcontroller "github.com/appmana/cloud-provisioning/controller/pkg/claim"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func verifyRealAPIReconciliation(t *testing.T, api client.Client) {
	t.Helper()
	ctx := context.Background()
	group := groupFixture()
	group.Name = "replica-loop"
	group.Spec.WorkloadSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "render-worker"}}
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
	if group.Status.Selector != "app=render-worker" || group.Status.Replicas != 3 || group.Status.PendingAction != nil || !slices.Contains(group.Finalizers, GroupFinalizer) || group.Status.ReadyReplicas != 0 {
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
	// Ensure the API schema retains resolved target identity across restarts.
	group.Status.PendingAction.NodeName = "reserved-node"
	group.Status.PendingAction.NodeUID = "reserved-node-uid"
	group.Status.PendingAction.MachineUID = "reserved-machine-uid"
	group.Status.PendingAction.ProviderID = "test:///reserved"
	if err := api.Status().Update(ctx, group); err != nil {
		t.Fatal(err)
	}
	observed := &v1alpha1.ProvisionedNodeGroupClaim{}
	if err := api.Get(ctx, request.NamespacedName, observed); err != nil {
		t.Fatal(err)
	}
	a := observed.Status.PendingAction
	if a.NodeName != "reserved-node" || a.NodeUID != "reserved-node-uid" || a.MachineUID != "reserved-machine-uid" || a.ProviderID != "test:///reserved" {
		t.Fatal("API pruned drain target", a)
	}

	requestCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "group-gateway-request", Namespace: group.Namespace, Annotations: map[string]string{"cloud-provisioning.appmana.com/gateway-mesh": "test-mesh"}}, Data: map[string]string{"request.json": `{"worker":{"uid":"reserved-machine-uid","nodeUID":"reserved-node-uid","providerID":"test:///reserved"}}`}}
	if err := api.Create(ctx, requestCM); err != nil {
		t.Fatal(err)
	}
	captured, err := CaptureGatewayInventory(ctx, api, observed, "test-mesh")
	if err != nil {
		t.Fatal(err)
	}
	if observed.Status.PendingAction.Gateways != nil {
		t.Fatal("inventory capture mutated caller")
	}
	if err := api.Get(ctx, request.NamespacedName, observed); err != nil {
		t.Fatal(err)
	}
	inventory := observed.Status.PendingAction.Gateways
	if inventory == nil || inventory.Mesh != "test-mesh" || len(inventory.Requests) != 1 || inventory.Requests[0].UID != string(requestCM.UID) {
		t.Fatal("API lost inventory", inventory)
	}
	copy := observed.DeepCopy()
	copy.Status.PendingAction.Gateways.Requests[0].UID = "changed"
	if observed.Status.PendingAction.Gateways.Requests[0].UID != string(requestCM.UID) {
		t.Fatal("inventory deepcopy aliases source")
	}
	newRequest := requestCM.DeepCopy()
	newRequest.Name += "-new"
	newRequest.UID = ""
	newRequest.ResourceVersion = ""
	if err := api.Create(ctx, newRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireGatewayInventory(ctx, api, captured); err == nil || !strings.Contains(err.Error(), "inventory changed") {
		t.Fatal("new attachment did not stop retirement", err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(requestCM), requestCM); err != nil || !requestCM.DeletionTimestamp.IsZero() {
		t.Fatal("changed requests before inventory was stable", err)
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
			// This child has crossed the claim controller's admission boundary.
			child.Finalizers = []string{claimcontroller.Finalizer}
			if err := api.Update(ctx, child); err != nil {
				t.Fatal(err)
			}
		}
		if err := api.Delete(ctx, group); err != nil {
			t.Fatal(err)
		}
		r := &Reconciler{API: api}
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		if created && err == nil {
			// Capacity status is committed before advancing the creation journal.
			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		}
		if created && err != nil {
			t.Fatal("existing child stuck during deletion", err)
		}
		if !created && err != nil {
			t.Fatal("unfulfilled reservation was not cancelled", err)
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
			if len(claims.Items) != 0 || group.Status.PendingAction != nil {
				t.Fatal("created capacity during group deletion")
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			if err := api.Get(ctx, key, group); !apierrors.IsNotFound(err) {
				t.Fatal("cancelled empty group retained finalizer", err)
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
