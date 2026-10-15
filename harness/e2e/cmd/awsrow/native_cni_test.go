package main

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestNativeCNIRejectsCompetingPlugins(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		pass bool
	}{
		{`{"plugins":[{"type":"aws-cni"},{"type":"egress-cni"},{"type":"portmap"}]}`, true},
		{`{"plugins":[{"type":"calico"},{"type":"aws-cni"}]}`, false},
		{`{"plugins":[{"type":"aws-cni"},{"type":"calico"}]}`, false},
		{`{"plugins":[{"type":"aws-cni"},{"type":"aws-cni"}]}`, false},
		{`{"type":"calico"}`, false},
		{`invalid`, false},
	} {
		_, err := nativePlugins([]byte(tc.raw))
		if (err == nil) != tc.pass {
			t.Errorf("%s: %v", tc.raw, err)
		}
	}
}

func TestNativeAgentOwnership(t *testing.T) {
	pod := func(node, agent string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"k8s-app": agent}},
			Spec:       corev1.PodSpec{NodeName: node},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
		}
	}
	for _, name := range []string{"healthy", "missing", "calico-on-aws", "aws-on-site", "duplicate", "unready", "terminating", "unlabelled-installer"} {
		t.Run(name, func(t *testing.T) {
			pods := corev1.PodList{Items: []corev1.Pod{pod("site", "calico-node"), pod("cloud", "aws-node")}}
			switch name {
			case "missing":
				pods.Items = pods.Items[:1]
			case "calico-on-aws":
				pods.Items = append(pods.Items, pod("cloud", "calico-node"))
			case "aws-on-site":
				pods.Items = append(pods.Items, pod("site", "aws-node"))
			case "duplicate":
				pods.Items = append(pods.Items, pod("cloud", "aws-node"))
			case "unready":
				pods.Items[1].Status.Conditions = nil
			case "terminating":
				now := metav1.Now()
				pods.Items[1].DeletionTimestamp = &now
			case "unlabelled-installer":
				p := pod("cloud", "")
				p.Spec.InitContainers = []corev1.Container{{Image: "docker.io/calico/cni:v3"}}
				pods.Items = append(pods.Items, p)
			}
			err := cniPodOwnership(pods, map[string]bool{"cloud": true}, map[string]bool{"site": true})
			if (err == nil) != (name == "healthy") {
				t.Fatalf("unexpected ownership verdict: %v", err)
			}
		})
	}
}
