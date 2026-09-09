package nodegroup

import (
	"context"
	"slices"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	claimcontroller "github.com/appmana/cloud-provisioning/controller/pkg/claim"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func cancellationFixture(t *testing.T, api client.Client, name string) *v1alpha1.ProvisionedNodeGroupClaim {
	t.Helper()
	ctx := context.Background()
	g := groupFixture()
	g.UID = ""
	g.Name = name
	g.Namespace = "default"
	g.Finalizers = []string{GroupFinalizer}
	if err := api.Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	a, err := ProposeAction(g, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err = ReserveAction(ctx, api, g, a)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func verifyCreationCancellationRaces(t *testing.T, api client.Client) {
	t.Helper()
	ctx := context.Background()
	for _, gone := range []bool{false, true} {
		name := "cancel-late-child"
		if gone {
			name += "-gone"
		}
		g := cancellationFixture(t, api, name)
		uid, id := g.UID, g.Status.PendingAction.ID
		key := client.ObjectKeyFromObject(g)
		stale := &creationRaceClient{Client: api, before: func() {
			if err := api.Delete(ctx, g); err != nil {
				t.Fatal(err)
			}
			current := &v1alpha1.ProvisionedNodeGroupClaim{}
			if err := api.Get(ctx, key, current); err != nil {
				t.Fatal(err)
			}
			if cancelled, err := CancelUnfulfilledCreation(ctx, api, current); err != nil || !cancelled {
				t.Fatal("cancellation failed", cancelled, err)
			}
			if gone {
				if err := api.Get(ctx, key, current); err != nil {
					t.Fatal(err)
				}
				current.Finalizers = nil
				if err := api.Update(ctx, current); err != nil {
					t.Fatal(err)
				}
			}
		}}
		child, err := ResumeCreation(ctx, stale, key, uid, id)
		if err != nil {
			t.Fatal("fixture did not produce late child", err)
		}
		// Envtest has no garbage collector: explicitly invoke the real claim
		// reconciler against the late child to verify that it cannot provision.
		claims := &claimcontroller.Reconciler{Client: api, Reader: api}
		if _, err := claims.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(child)}); err != nil {
			t.Fatal(err)
		}
		if err := api.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(child.Finalizers, claimcontroller.Finalizer) {
			t.Fatal("late child crossed admission boundary")
		}
		if !gone {
			r := &Reconciler{API: api}
			for i := 0; i < 3; i++ {
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
					t.Fatal(err)
				}
			}
			if err := api.Get(ctx, client.ObjectKeyFromObject(child), child); !apierrors.IsNotFound(err) {
				t.Fatal("late child survived cancellation", err)
			}
			if err := api.Get(ctx, key, g); !apierrors.IsNotFound(err) {
				t.Fatal("cancelled group retained finalizer", err)
			}
		} else {
			if err := api.Delete(ctx, child); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Admission winning the child resource-version race must retain its
	// lifecycle. Never remove a newly installed provisioning finalizer.
	g := cancellationFixture(t, api, "cancel-admission-race")
	child, err := ResumeCreation(ctx, api, client.ObjectKeyFromObject(g), g.UID, g.Status.PendingAction.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
		t.Fatal(err)
	}
	racer := &admissionRaceClient{Client: api, before: func() {
		current := &v1alpha1.ProvisionedNodeClaim{}
		if err := api.Get(ctx, client.ObjectKeyFromObject(child), current); err != nil {
			t.Fatal(err)
		}
		current.Finalizers = append(current.Finalizers, claimcontroller.Finalizer)
		if err := api.Update(ctx, current); err != nil {
			t.Fatal(err)
		}
	}}
	if changed, err := cancelUnstartedChildren(ctx, racer, g, []v1alpha1.ProvisionedNodeClaim{*child}); err == nil || changed {
		t.Fatal("stale cancellation crossed admission", changed, err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(child.Finalizers, claimcontroller.Finalizer) || !child.DeletionTimestamp.IsZero() {
		t.Fatal("admitted child was deleted")
	}
	if cancelled, err := CancelUnfulfilledCreation(ctx, api, g); err != nil || cancelled {
		t.Fatal("existing child reservation was cancelled", cancelled, err)
	}
	if changed, err := cancelUnstartedChildren(ctx, api, g, []v1alpha1.ProvisionedNodeClaim{*child}); err != nil || changed {
		t.Fatal("admitted child bypassed lifecycle", changed, err)
	}
}

type creationRaceClient struct {
	client.Client
	before func()
}

func (c *creationRaceClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.before()
	return c.Client.Create(ctx, obj, opts...)
}

type admissionRaceClient struct {
	client.Client
	before func()
}

func (c *admissionRaceClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.before()
	return c.Client.Delete(ctx, obj, opts...)
}
