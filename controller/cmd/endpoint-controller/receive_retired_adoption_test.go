package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Real journal API with synthetic owners, native receipts and TokenReview.
// Exercises the reconciliation entry point and conditional deletion races;
// does not establish cloud termination, native absence or workload continuity.
func verifyRetiredAdoptionCleanup(t *testing.T, r *meshReconciler, owners client.Client, api kubernetes.Interface, mesh *corev1.Secret, store handover.Store, plan tunnel.ReceiveTransitionPlan, policy receiveReceiptPolicy, retiredOwners []client.Object, complete func()) {
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	adoptionKey := client.ObjectKey{Namespace: plan.Namespace, Name: plan.AdoptionName}
	local := *r
	preparation, journal := *r.receivePreparation, *r.receivePreparation.Journal
	journal.Receipts = &policy
	preparation.Journal = &journal
	local.receivePreparation, local.receiveAPI = &preparation, api
	local.Client, local.reader = owners, owners
	for _, fault := range []string{"disabled", "missing-completion", "expired-completion", "claim-present", "node-present", "machine-present", "read-error", "replacement-secret", "source-race", "delete-update-race", "cancelled", "success"} {
		t.Run(fault, func(t *testing.T) {
			adoption := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: plan.AdoptionName, Namespace: plan.Namespace, UID: types.UID(plan.AdoptionUID)}}
			must(owners.Create(ctx, adoption))
			t.Cleanup(func() {
				current := &corev1.Secret{}
				if err := owners.Get(ctx, adoptionKey, current); err == nil {
					must(owners.Delete(ctx, current))
				} else if !apierrors.IsNotFound(err) {
					t.Error(err)
				}
			})
			complete()
			_, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, plan.Namespace, plan.MeshName, plan.MeshUID)
			must(err)
			saved := index.DeepCopy()
			t.Cleanup(func() {
				current, err := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
				must(err)
				current.Data, current.Annotations = saved.Data, saved.Annotations
				_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
				must(err)
			})
			test := local
			callCtx := ctx
			switch fault {
			case "disabled":
				p, j, receipt := preparation, journal, policy
				receipt.FinalizeRetirements = false
				j.Receipts = &receipt
				p.Journal = &j
				test.receivePreparation = &p
			case "missing-completion", "expired-completion":
				var inventory tunnel.ReceiveInventory
				must(json.Unmarshal([]byte(index.Data["inventory.json"]), &inventory))
				pub := inventory.Publications[plan.MachineUID]
				if fault == "missing-completion" {
					pub.RetirementCompletion = nil
				} else {
					var completion map[string]any
					must(json.Unmarshal(pub.RetirementCompletion, &completion))
					for _, entry := range completion["receipts"].([]any) {
						entry.(map[string]any)["observedAt"] = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
					}
					pub.RetirementCompletion, err = json.Marshal(completion)
					must(err)
				}
				inventory.Publications[plan.MachineUID] = pub
				raw, err := json.Marshal(inventory)
				must(err)
				index.Data["inventory.json"] = string(raw)
				_, err = store.ConfigMaps.Update(ctx, index, metav1.UpdateOptions{})
				must(err)
			case "claim-present", "machine-present", "node-present":
				i := map[string]int{"claim-present": 0, "machine-present": 1, "node-present": 2}[fault]
				owner := retiredOwners[i].DeepCopyObject().(client.Object)
				owner.SetResourceVersion("")
				owner.SetUID("replacement-owner")
				must(owners.Create(ctx, owner))
				t.Cleanup(func() { must(owners.Delete(ctx, owner)) })
			case "read-error":
				test.reader = retirementUnavailableReader{Reader: owners}
			case "replacement-secret":
				must(owners.Delete(ctx, adoption))
				adoption.UID = "replacement"
				adoption.ResourceVersion = ""
				must(owners.Create(ctx, adoption))
			case "source-race":
				reads := 0
				test.reader = retiredAdoptionRaceReader{Reader: owners, before: func(object client.Object) error {
					if _, ok := object.(*unstructured.Unstructured); !ok {
						return nil
					}
					reads++
					if reads != 2 {
						return nil
					} // Initial absence, then write-boundary absence.
					current, err := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					current.Annotations = map[string]string{"test-source-race": "changed"}
					_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
					return err
				}}
			case "delete-update-race":
				test.Client = retiredAdoptionRaceClient{Client: owners, before: func() error {
					current := &corev1.Secret{}
					if err := owners.Get(ctx, adoptionKey, current); err != nil {
						return err
					}
					current.Annotations = map[string]string{"test-update-race": "changed"}
					return owners.Update(ctx, current)
				}}
			case "cancelled":
				var cancel context.CancelFunc
				callCtx, cancel = context.WithCancel(ctx)
				cancel()
			}
			progress, err := test.reconcileRetiredReceiveAdoptions(callCtx)
			if fault == "success" {
				must(err)
				if !progress {
					t.Fatal("normal retirement left captured adoption behind")
				}
				if err := owners.Get(ctx, adoptionKey, &corev1.Secret{}); !apierrors.IsNotFound(err) {
					t.Fatal("captured adoption remained", err)
				}
				again, err := test.reconcileRetiredReceiveAdoptions(ctx)
				must(err)
				if again {
					t.Fatal("completed adoption cleanup repeated a mutation")
				}
			} else {
				if progress {
					t.Fatal("failed admission reported deletion", fault)
				}
				must(owners.Get(ctx, adoptionKey, &corev1.Secret{}))
				if fault != "disabled" && fault != "missing-completion" && fault != "claim-present" && fault != "machine-present" && fault != "node-present" && err == nil {
					t.Fatal("unsafe adoption cleanup was not rejected", fault)
				}
			}
			if fault == "expired-completion" {
				result, err := test.finishRetiredReceiveAdoptions(ctx, ctrl.Result{RequeueAfter: 30 * time.Second})
				must(err)
				if result.RequeueAfter != 5*time.Second {
					t.Fatalf("expired evidence did not schedule a bounded refresh: %+v", result)
				}
				must(owners.Get(ctx, adoptionKey, &corev1.Secret{}))
				// Only a newly verified completion permits the next pass to delete.
				complete()
				index, err = store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
				must(err)
				result, err = test.finishRetiredReceiveAdoptions(ctx, ctrl.Result{})
				must(err)
				if result.RequeueAfter != time.Second {
					t.Fatalf("refreshed cleanup did not report progress: %+v", result)
				}
				if err := owners.Get(ctx, adoptionKey, &corev1.Secret{}); !apierrors.IsNotFound(err) {
					t.Fatal("fresh completion failed to retire captured adoption", err)
				}
			}
			current, err := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
			must(err)
			if current.Data["inventory.json"] != index.Data["inventory.json"] {
				t.Fatal("adoption cleanup changed reservation or evidence")
			}
		})
	}
}

type retiredAdoptionRaceReader struct {
	client.Reader
	before func(client.Object) error
}

func (r retiredAdoptionRaceReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if err := r.before(object); err != nil {
		return err
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

type retiredAdoptionRaceClient struct {
	client.Client
	before func() error
}

func (c retiredAdoptionRaceClient) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	if _, ok := object.(*corev1.Secret); !ok {
		return fmt.Errorf("unexpected deletion")
	}
	if err := c.before(); err != nil {
		return err
	}
	return c.Client.Delete(ctx, object, opts...)
}

// Unlike the fake client's delete implementation, the real API must reject a
// replaced UID even if the caller still holds the original object. This checks
// deletion preconditions only; cleanup evidence is covered by the lifecycle test.
func verifyRetiredAdoptionDeletePreconditions(t *testing.T, api client.Client, namespace string) {
	ctx := context.Background()
	r := &meshReconciler{Client: api}
	for _, state := range []string{"unchanged", "updated", "recreated", "already-gone"} {
		t.Run(state, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{GenerateName: "adoption-cas-", Namespace: namespace}}
			must(api.Create(ctx, secret))
			captured := secret.DeepCopy()
			key := client.ObjectKeyFromObject(secret)
			t.Cleanup(func() {
				current := &corev1.Secret{}
				if err := api.Get(ctx, key, current); err == nil {
					must(api.Delete(ctx, current))
				} else if !apierrors.IsNotFound(err) {
					t.Error(err)
				}
			})
			switch state {
			case "updated":
				secret.Annotations = map[string]string{"changed": "true"}
				must(api.Update(ctx, secret))
			case "recreated", "already-gone":
				must(api.Delete(ctx, secret))
				if state == "recreated" {
					secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: captured.Name, Namespace: namespace}}
					must(api.Create(ctx, secret))
					if secret.UID == captured.UID {
						t.Fatal("replacement fixture kept UID")
					}
				}
			}
			progress, err := r.deleteCapturedReceiveAdoption(ctx, captured)
			if state == "updated" || state == "recreated" {
				if progress || !apierrors.IsConflict(err) {
					t.Fatal("stale conditional delete accepted", state, progress, err)
				}
				current := &corev1.Secret{}
				must(api.Get(ctx, key, current))
				if current.UID != secret.UID || current.ResourceVersion != secret.ResourceVersion {
					t.Fatal("conflicting deletion changed replacement")
				}
			} else {
				must(err)
				if !progress {
					t.Fatal("captured deletion made no progress")
				}
				if err := api.Get(ctx, key, &corev1.Secret{}); !apierrors.IsNotFound(err) {
					t.Fatal("captured adoption remains", err)
				}
			}
		})
	}
}
