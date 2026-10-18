package main

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Source comparison only: this does not prove native cleanup or receipt
// completion. Only resource-version, managed-field and heartbeat churn is
// permitted; readiness transitions and all forwarding inputs remain fenced.
func TestReceiveParticipantHeartbeatComparison(t *testing.T) {
	now := metav1.Now()
	before := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node-uid", ResourceVersion: "1"},
		Spec:       corev1.NodeSpec{PodCIDR: "10.42.0.0/24"},
		Status: corev1.NodeStatus{
			NodeInfo:   corev1.NodeSystemInfo{BootID: "boot-1"},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.1"}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: now, LastTransitionTime: now}},
		},
	}
	heartbeat := before.DeepCopy()
	heartbeat.ResourceVersion = "2"
	heartbeat.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(now.Add(time.Second))
	heartbeat.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubelet", Time: &now}}
	if !sameReceiveParticipantNode(before, heartbeat) {
		t.Fatal("routine heartbeat invalidated retirement source")
	}
	if before.ResourceVersion != "1" || !before.Status.Conditions[0].LastHeartbeatTime.Equal(&now) || heartbeat.ResourceVersion != "2" {
		t.Fatal("comparison mutated its source")
	}
	for name, change := range map[string]func(*corev1.Node){
		"replacement":     func(n *corev1.Node) { n.UID = "replacement" },
		"reboot":          func(n *corev1.Node) { n.Status.NodeInfo.BootID = "boot-2" },
		"not-ready":       func(n *corev1.Node) { n.Status.Conditions[0].Status = corev1.ConditionFalse },
		"readiness-cycle": func(n *corev1.Node) { n.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(time.Second)) },
		"address":         func(n *corev1.Node) { n.Status.Addresses[0].Address = "192.0.2.2" },
		"allocation":      func(n *corev1.Node) { n.Spec.PodCIDR = "10.43.0.0/24" },
		"annotation":      func(n *corev1.Node) { n.Annotations = map[string]string{"authority": "changed"} },
		"deletion":        func(n *corev1.Node) { n.DeletionTimestamp = &now },
		"missing-version": func(n *corev1.Node) { n.ResourceVersion = "" },
	} {
		t.Run(name, func(t *testing.T) {
			after := heartbeat.DeepCopy()
			change(after)
			if sameReceiveParticipantNode(before, after) {
				t.Fatal("changed participant source admitted")
			}
		})
	}
}
