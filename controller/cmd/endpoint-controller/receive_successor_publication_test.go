package main

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"net/netip"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Real archive/journal/inventory storage; Role/Secret writes and readiness use
// the existing simulated fixture. Missing Role access and an interrupted journal
// write must preserve serving history with zero permitted interruption. Retry
// uses the same archive and journal UID; no rollback is needed before CAS.
// Synthetic round times do not qualify recovery deadlines, RBAC propagation,
// agent observation or traffic continuity after publication.
func verifySuccessorPublication(t *testing.T, r *meshReconciler, machine *unstructured.Unstructured, native []netip.Prefix, ports []int, previous handover.Snapshot) {
	t.Helper()
	ctx := context.Background()
	store := r.receiveJournal
	store.Name = tunnel.ReceiveTransitionName(string(machine.GetUID()))
	withoutProposal := *r
	withoutProposal.reader = fake.NewClientBuilder().WithScheme(r.Scheme()).Build()
	if name, err := withoutProposal.receivePreparationArchive(ctx, store, previous); err != nil || name != "" {
		t.Fatal("retained journal without proposal requested new archive access", err)
	}
	original, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		current, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		current.Data = original.DeepCopy().Data
		if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}()
	if err := rbacv1.AddToScheme(r.Scheme()); err != nil {
		t.Fatal(err)
	}
	oldAccount := r.dialerServiceAccount
	r.dialerServiceAccount = "successor-dialer"
	defer func() { r.dialerServiceAccount = oldAccount }()
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: r.dialerServiceAccount, Namespace: r.secretNamespace}}
	defer func() {
		if err := r.Delete(ctx, role); err != nil {
			t.Error(err)
		}
	}()
	policy, err := r.receivePreparation.Journal.policy()
	if err != nil {
		t.Fatal(err)
	}
	view, err := previous.Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	at := view.Round.IssuedAt.Add(time.Minute)
	assertUnchanged := func() {
		t.Helper()
		current, err := store.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		oldRaw, _ := previous.Transition().Bytes()
		newRaw, _ := current.Transition().Bytes()
		if current.Metadata().UID != previous.Metadata().UID || !bytes.Equal(current.Plan(), previous.Plan()) || !bytes.Equal(newRaw, oldRaw) {
			t.Fatal("failed successor publication changed serving journal")
		}
	}
	if next, err := r.ensureReceiveTransition(ctx, machine, store, native, ports, policy, at); err == nil || next != nil {
		t.Fatal("missing Role allowed successor publication")
	}
	assertUnchanged()
	if err := r.Create(ctx, role); err != nil {
		t.Fatal(err)
	}
	baseClient := r.Client
	r.Client = &successorRoleRaceClient{Client: baseClient, role: client.ObjectKeyFromObject(role)}
	raceErr := r.ensureDialerRole(ctx)
	r.Client = baseClient
	if !apierrors.IsConflict(raceErr) {
		t.Fatal("older Role derivation overwrote a concurrent archive grant", raceErr)
	}
	concurrent := &rbacv1.Role{}
	if err := r.reader.Get(ctx, client.ObjectKeyFromObject(role), concurrent); err != nil {
		t.Fatal(err)
	}
	if len(concurrent.Rules) != 1 || !slices.Contains(concurrent.Rules[0].ResourceNames, "concurrent-archive") {
		t.Fatal("concurrent archive grant was lost")
	}
	archive, err := store.ArchiveCompleted(ctx, previous)
	if err != nil {
		t.Fatal(err)
	}
	checkGrant := func() error {
		current := &rbacv1.Role{}
		if err := r.reader.Get(ctx, client.ObjectKeyFromObject(role), current); err != nil {
			return err
		}
		found := false
		for _, rule := range current.Rules {
			if !slices.Contains(rule.Resources, "configmaps") {
				continue
			}
			if !slices.Equal(rule.Verbs, []string{"get"}) || len(rule.ResourceNames) == 0 || slices.Contains(rule.ResourceNames, "*") {
				return fmt.Errorf("archive access is broader than named reads")
			}
			found = found || slices.Contains(rule.ResourceNames, archive.Metadata().Name)
		}
		if !found {
			return fmt.Errorf("successor CAS preceded archive read access")
		}
		return nil
	}
	calls, failWrite := 0, true
	checked := store
	checked.ConfigMaps = inventoryRacingUpdate{ConfigMapInterface: store.ConfigMaps, before: func() error {
		calls++
		if err := checkGrant(); err != nil {
			return err
		}
		if failWrite {
			return fmt.Errorf("injected successor journal write failure")
		}
		return nil
	}}
	if next, err := r.ensureReceiveTransition(ctx, machine, checked, native, ports, policy, at); err == nil || next != nil || calls != 1 {
		t.Fatal("journal interruption did not reach CAS", err, calls)
	}
	assertUnchanged()
	// A normal Role pass between attempts must keep the pending archive grant.
	if err := r.ensureDialerRole(ctx); err != nil {
		t.Fatal(err)
	}
	if err := checkGrant(); err != nil {
		t.Fatal(err)
	}
	failWrite = false
	next, err := r.ensureReceiveTransition(ctx, machine, checked, native, ports, policy, at)
	if err != nil || next == nil || calls != 2 {
		t.Fatal("successor publication retry failed", err, calls)
	}
	if next.Metadata().UID != previous.Metadata().UID {
		t.Fatal("successor replaced journal identity")
	}
	ref, found := next.Predecessor()
	if !found || ref.ArchiveName != archive.Metadata().Name || ref.ArchiveUID != string(archive.Metadata().UID) {
		t.Fatal("retry did not preserve terminal archive identity")
	}
	if err := tunnel.ValidateReceiveTransportHistory(ctx, store, *next); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ArchiveNames(ctx, *next); err != nil {
		t.Fatal(err)
	}
	newView, err := next.Transition().View()
	if err != nil || newView.Complete || newView.Round.Phase != handover.Prepare || newView.Round.FromGeneration != view.ServingGeneration {
		t.Fatal("successor did not start from serving transport", err)
	}
	resumed, err := r.ensureReceiveTransition(ctx, machine, store, nil, nil, handover.Policy{}, at)
	if err != nil || resumed == nil || resumed.Metadata().ResourceVersion != next.Metadata().ResourceVersion {
		t.Fatal("retry rewrote the published successor", err)
	}
	if err := r.ensureDialerRole(ctx); err != nil {
		t.Fatal(err)
	}
	if err := checkGrant(); err != nil {
		t.Fatal(err)
	}
}

// Inject a newer Role grant after the reconciler begins deriving membership.
type successorRoleRaceClient struct {
	client.Client
	role client.ObjectKey
}

func (c *successorRoleRaceClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	role := &rbacv1.Role{}
	if err := c.Client.Get(ctx, c.role, role); err != nil {
		return err
	}
	role.Rules = []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}, ResourceNames: []string{"concurrent-archive"}}}
	if err := c.Client.Update(ctx, role); err != nil {
		return err
	}
	return c.Client.List(ctx, list, opts...)
}
