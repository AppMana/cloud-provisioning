package nodegroup

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
)

func verifyRealAPIDrain(t *testing.T, api client.Client) {
	t.Helper()
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "group-drain-worker"}}
	if err := api.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	if _, err := DrainNode(ctx, api, node.Name, "previous-node"); err == nil {
		t.Fatal("cordoned replacement node")
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		t.Fatal(err)
	}
	if node.Spec.Unschedulable {
		t.Fatal("replacement node changed")
	}
	zero := int64(0)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "render-job", Namespace: "default", Labels: map[string]string{"app": "drain-test"}}, Spec: corev1.PodSpec{NodeName: node.Name, TerminationGracePeriodSeconds: &zero, Containers: []corev1.Container{{Name: "worker", Image: "test.invalid/worker"}}}}
	if err := api.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := api.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	one := intstr.FromInt32(1)
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "render-jobs", Namespace: "default"}, Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "drain-test"}}}}
	if err := api.Create(ctx, pdb); err != nil {
		t.Fatal(err)
	}
	pdb.Status.ObservedGeneration = pdb.Generation
	pdb.Status.CurrentHealthy = 1
	pdb.Status.DesiredHealthy = 1
	pdb.Status.ExpectedPods = 1
	if err := api.Status().Update(ctx, pdb); err != nil {
		t.Fatal(err)
	}
	if empty, err := DrainNode(ctx, api, node.Name, node.UID); err != nil || empty {
		t.Fatal("cordon", empty, err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		t.Fatal(err)
	}
	if !node.Spec.Unschedulable {
		t.Fatal("node still schedulable")
	}
	if empty, err := DrainNode(ctx, api, node.Name, node.UID); err != nil || empty {
		t.Fatal("PDB should block drain", empty, err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal("PDB-protected pod lost", err)
	}
	if !pod.DeletionTimestamp.IsZero() {
		t.Fatal("PDB bypassed")
	}
	// Simulate the disruption controller observing replacement capacity.
	pdb.Status.DisruptionsAllowed = 1
	pdb.Status.CurrentHealthy = 2
	pdb.Status.ExpectedPods = 2
	if err := api.Status().Update(ctx, pdb); err != nil {
		t.Fatal(err)
	}
	if empty, err := DrainNode(ctx, api, node.Name, node.UID); err != nil || empty {
		t.Fatal("eviction", empty, err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(pod), pod); !apierrors.IsNotFound(err) {
		t.Fatalf("evicted pod remains: %v", err)
	}
	// Keep network DaemonSet pods running throughout workload drain.
	controller := true
	agent := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "network-agent", Namespace: "default", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "network", UID: "network-controller", Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: node.Name, Containers: []corev1.Container{{Name: "agent", Image: "test.invalid/agent"}}}}
	if err := api.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if empty, err := DrainNode(ctx, api, node.Name, node.UID); err != nil || !empty {
		t.Fatal("drain did not finish", empty, err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal("network agent evicted", err)
	}

}
