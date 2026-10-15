package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Called with the controller's fully validated preparation fixture. Use real
// API storage to verify the adapter commits and resumes the exact public plan.
func verifyReceiveTransitionJournal(t *testing.T, r *meshReconciler, machine *unstructured.Unstructured, native []netip.Prefix, ports []int) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	k, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := k.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: machine.GetNamespace()}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	if err := r.configureReceiveJournal(ctx, k); err != nil {
		t.Fatal(err)
	}
	r.receivePreparation.Journal = &receiveJournalPolicy{RoundLifetime: metav1.Duration{Duration: time.Minute}, MaxReceiptAge: metav1.Duration{Duration: 10 * time.Second}, DrainInterval: metav1.Duration{Duration: 5 * time.Second}}
	defer func() { r.receivePreparation.Journal = nil }()
	store := r.receiveJournal
	store.Name = receiveTransitionName(string(machine.GetUID()))
	now := time.Now()
	if err := r.ensureAdoptionConfig(ctx, machine); err != nil {
		t.Fatalf("normal adoption reconciliation did not commit a prepared plan: %v", err)
	}
	snapshot, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view, err := snapshot.Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Round.Phase != handover.Prepare || view.Complete || len(view.Round.Members) != 2 {
		t.Fatal("journal did not begin with both transport endpoints in prepare")
	}
	var plan tunnel.ReceiveTransitionPlan
	if err := json.Unmarshal(snapshot.Plan(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.MachineUID != string(machine.GetUID()) || len(plan.Sites) != 1 {
		t.Fatal("journal lost controller plan")
	}
	if err := r.ensureAdoptionConfig(ctx, machine); err != nil {
		t.Fatalf("normal reconciliation could not resume its journal: %v", err)
	}
	resumed, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resumedView, err := resumed.Transition().View()
	if err != nil || resumedView.Round.Nonce != view.Round.Nonce || !bytes.Equal(snapshot.Plan(), resumed.Plan()) {
		t.Fatal("normal reconciliation replaced the committed plan or round")
	}
	// An expired observation round remains the same persisted round on restart.
	// No new policy or missing transient preparation input may silently renew it.
	restored, err := r.ensureReceiveTransition(ctx, machine, store, nil, nil, handover.Policy{}, now.Add(time.Hour))
	if err != nil || restored == nil {
		t.Fatalf("restart could not restore the committed plan: %v", err)
	}
	restoredView, err := restored.Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	if restoredView.Round.Nonce != view.Round.Nonce || !bytes.Equal(snapshot.Plan(), restored.Plan()) {
		t.Fatal("restart reconstructed a different round or plan")
	}
	replaced := machine.DeepCopy()
	replaced.SetUID("replacement-machine")
	if _, err := r.ensureReceiveTransition(ctx, replaced, store, native, ports, handover.Policy{}, now); err == nil {
		t.Fatal("replacement machine adopted another journal")
	}
	object, err := k.CoreV1().ConfigMaps(machine.GetNamespace()).Get(ctx, store.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	object.Data["plan.json"] = "{}"
	if _, err := k.CoreV1().ConfigMaps(machine.GetNamespace()).Update(ctx, object, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureAdoptionConfig(ctx, machine); err == nil {
		t.Fatal("normal reconciliation accepted a corrupted journal plan")
	}
}
