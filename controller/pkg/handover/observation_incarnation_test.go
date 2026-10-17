package handover

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"testing"
	"time"
)

func TestObservationIncarnationRejectsRestartAndReplacement(t *testing.T) {
	now := time.Now()
	original := Receipt{NodeUID: "node-uid", ObservedBootID: "boot", AgentPodName: "agent", AgentPodUID: "pod-uid", AgentContainerName: "dialer", AgentContainerID: "container", ObservedAt: now}
	for _, fault := range []string{"healthy", "old-journal", "boot", "node-uid", "node-unready", "node-deleting", "pod-uid", "pod-node", "pod-unready", "pod-deleting", "pod-pending", "container", "container-unready", "container-stopped", "container-start", "missing-pod", "missing-node"} {
		t.Run(fault, func(t *testing.T) {
			r := original
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "relay", UID: "node-uid"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "system", Name: "agent", UID: "pod-uid"}, Spec: corev1.PodSpec{NodeName: "relay"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "dialer", ContainerID: "container", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Minute))}}}}}}
			stamp := metav1.Now()
			switch fault {
			case "old-journal":
				r.ObservedBootID = ""
			case "boot":
				node.Status.NodeInfo.BootID = "new"
			case "node-uid":
				node.UID = "new"
			case "node-unready":
				node.Status.Conditions = nil
			case "node-deleting":
				node.DeletionTimestamp = &stamp
			case "pod-uid":
				pod.UID = "new"
			case "pod-node":
				pod.Spec.NodeName = "other"
			case "pod-unready":
				pod.Status.Conditions = nil
			case "pod-deleting":
				pod.DeletionTimestamp = &stamp
			case "pod-pending":
				pod.Status.Phase = corev1.PodPending
			case "container":
				pod.Status.ContainerStatuses[0].ContainerID = "new"
			case "container-unready":
				pod.Status.ContainerStatuses[0].Ready = false
			case "container-stopped":
				pod.Status.ContainerStatuses[0].State.Running = nil
			case "container-start":
				pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(now.Add(time.Second))
			case "missing-pod":
				pod.Name = "other"
			case "missing-node":
				node.Name = "other"
			}
			err := VerifyObservationIncarnation(context.Background(), fake.NewClientset(node, pod), "system", "relay", r)
			if (err == nil) != (fault == "healthy") {
				t.Fatalf("unexpected dependency admission: %v", err)
			}
		})
	}
}
