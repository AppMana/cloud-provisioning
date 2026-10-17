package handover

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// VerifyObservationIncarnation rechecks an already authenticated journal
// observation before using it as a forwarding dependency. It does not
// authenticate a submission or prove current forwarding by itself. Callers
// must first admit the journal and validate the observation's round and age.
// Missing identity in an older journal is not evidence of the current boot.
func VerifyObservationIncarnation(ctx context.Context, api kubernetes.Interface, namespace, nodeName string, receipt Receipt) error {
	fail := func() error {
		return fmt.Errorf("relay forwarding dependency: observed agent incarnation is not current")
	}
	if api == nil || namespace == "" || nodeName == "" || receipt.NodeUID == "" || receipt.ObservedBootID == "" || receipt.AgentPodName == "" || receipt.AgentPodUID == "" || receipt.AgentContainerName == "" || receipt.AgentContainerID == "" || receipt.ObservedAt.IsZero() {
		return fail()
	}
	node, err := api.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil || string(node.UID) != receipt.NodeUID || node.DeletionTimestamp != nil || node.Status.NodeInfo.BootID != receipt.ObservedBootID {
		return fail()
	}
	ready := false
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	if !ready {
		return fail()
	}
	pod, err := api.CoreV1().Pods(namespace).Get(ctx, receipt.AgentPodName, metav1.GetOptions{})
	if err != nil || string(pod.UID) != receipt.AgentPodUID || pod.DeletionTimestamp != nil || pod.Spec.NodeName != nodeName || pod.Status.Phase != corev1.PodRunning {
		return fail()
	}
	ready = false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	if !ready {
		return fail()
	}
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == receipt.AgentContainerName && s.ContainerID == receipt.AgentContainerID && s.Ready && s.State.Running != nil && !s.State.Running.StartedAt.IsZero() && !receipt.ObservedAt.Before(s.State.Running.StartedAt.Time) {
			return nil
		}
	}
	return fail()
}
