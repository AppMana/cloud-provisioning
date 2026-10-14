package main

import (
	"context"
	appsv1 "k8s.io/api/apps/v1"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func TestSiteDialerAdmissionBoundsPrivilegedPlacement(t *testing.T) {
	selector, err := labels.Parse("cloud-provisioning.appmana.com/site-dialer=enabled")
	if err != nil {
		t.Fatal(err)
	}
	r := &meshReconciler{siteDialerSelector: selector, tunnelEndpointSelector: labels.SelectorFromSet(labels.Set{"kubernetes.io/hostname": "canary"})}
	node := func(name, os, role string, admitted bool) corev1.Node {
		l := map[string]string{"kubernetes.io/hostname": name, "kubernetes.io/os": os}
		if admitted {
			l["cloud-provisioning.appmana.com/site-dialer"] = "enabled"
		}
		if role != "" {
			l[cloudWorkerRoleLabel] = role
		}
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
	}
	nodes := []corev1.Node{node("canary", "linux", "", true), node("other", "linux", "", false), node("windows", "windows", "", true), node("remote", "linux", cloudWorkerRoleValue, true)}
	for i := range nodes {
		if got := matchesNode(r.siteDialerNodeAffinity(), &nodes[i]); got != (i == 0) {
			t.Errorf("dialer on %s = %v", nodes[i].Name, got)
		}
	}
	if err := r.validateSiteDialerAdmission(nodes); err != nil {
		t.Fatal(err)
	}
	delete(nodes[0].Labels, "cloud-provisioning.appmana.com/site-dialer")
	if err := r.validateSiteDialerAdmission(nodes); err == nil {
		t.Fatal("endpoint outside admission accepted")
	}
}

func TestPinnedAdmissionSurvivesLabelRemoval(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "canary", Labels: map[string]string{"kubernetes.io/os": "linux", "admitted": "yes"}}}
	r, c := dualStackReconciler(t, "", n)
	r.siteDialerSelector = labels.SelectorFromSet(labels.Set{"admitted": "yes"})
	desired := &appsv1.DaemonSet{Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}}}}}}
	if err := r.pinSiteDialerAdmission(context.Background(), desired, nil); err != nil {
		t.Fatal(err)
	}
	old := desired.DeepCopy()
	delete(n.Labels, "admitted")
	if err := c.Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if err := r.pinSiteDialerAdmission(context.Background(), desired, old); err != nil {
		t.Fatal(err)
	}
	if !matchesNode(desired.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution, n) {
		t.Fatal("label removal evicted transit agent")
	}
}
