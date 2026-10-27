package main

import (
	appsv1 "k8s.io/api/apps/v1"
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
	for _, name := range []string{"healthy", "tunnel-agent", "missing", "calico-on-aws", "aws-on-site", "duplicate", "unready", "terminating", "unlabelled-installer"} {
		t.Run(name, func(t *testing.T) {
			pods := corev1.PodList{Items: []corev1.Pod{pod("site", "calico-node"), pod("cloud", "aws-node")}}
			switch name {
			case "tunnel-agent":
				// A tunnel-class node on the site network runs the product's
				// Calico agent instead of the site's.
				pods.Items[0].Labels["k8s-app"] = "calico-node-tunnel"
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
			if (err == nil) != (name == "healthy" || name == "tunnel-agent") {
				t.Fatalf("unexpected ownership verdict: %v", err)
			}
		})
	}
}

// A node's network comes from the class label the product registered it
// with, never from how the harness launched it.
func TestNodeNetworksAreReadFromTheRegisteredClass(t *testing.T) {
	list := corev1.NodeList{Items: []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "site-1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "aws-1", Labels: map[string]string{"cloud-provisioning.appmana.com/cni": "aws-vpc"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "tunnel-1", Labels: map[string]string{"cloud-provisioning.appmana.com/role": "cloud-worker"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "unmeasured", Labels: map[string]string{"cloud-provisioning.appmana.com/cni": "aws-vpc"}}},
	}}
	native, site := nodeNetworks(list, []string{"site-1", "aws-1", "tunnel-1"})
	if len(native) != 1 || !native["aws-1"] || len(site) != 2 || !site["site-1"] || !site["tunnel-1"] {
		t.Fatalf("native=%v site=%v", native, site)
	}
}

// aws-node must be the product's: applied by its field manager, not by an
// operator or the harness.
func TestAWSNodeMustBeTheProducts(t *testing.T) {
	for _, tc := range []struct {
		managers []metav1.ManagedFieldsEntry
		labels   map[string]string
		ok       bool
	}{
		{[]metav1.ManagedFieldsEntry{{Manager: "cloud-provisioning", Operation: metav1.ManagedFieldsOperationApply}}, map[string]string{"app.kubernetes.io/managed-by": "cloud-provisioning"}, true},
		{[]metav1.ManagedFieldsEntry{{Manager: "helm", Operation: metav1.ManagedFieldsOperationUpdate}}, map[string]string{"app.kubernetes.io/managed-by": "Helm"}, false},
		{[]metav1.ManagedFieldsEntry{{Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate}}, map[string]string{"app.kubernetes.io/managed-by": "cloud-provisioning"}, false},
	} {
		ds := appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "aws-node", Namespace: "kube-system", ManagedFields: tc.managers, Labels: tc.labels}}
		if err := awsNodeOwnedByProduct(ds); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.managers, err)
		}
	}
}
