package site

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func hostPathVolume(name, path string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path}}}
}

// k0s's calico-node: an OS nodeSelector, every taint tolerated, the host CNI
// directories mounted, and no affinity.
func calicoNode() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "calico-node", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			NodeSelector: map[string]string{corev1.LabelOSStable: "linux"},
			Tolerations:  []corev1.Toleration{{Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpExists}},
			Volumes:      []corev1.Volume{hostPathVolume("cni-net-dir", "/etc/cni/net.d"), hostPathVolume("cni-bin-dir", "/opt/cni/bin")},
		}}},
	}
}

func excluder(t *testing.T, objs ...client.Object) (*Excluder, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(installationGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(installationGVK.GroupVersion().WithKind("InstallationList"), &unstructured.UnstructuredList{})
	for _, gvk := range []schema.GroupVersionKind{bgpConfigurationGVK, bgpPeerGVK, ipPoolGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Excluder{Client: c, Reader: c, Native: []string{"aws-vpc"}, Now: time.Now}, c
}

func get(t *testing.T, c client.Client, namespace, name string) *appsv1.DaemonSet {
	t.Helper()
	ds := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, ds); err != nil {
		t.Fatal(err)
	}
	return ds
}

var nativeClass = platform.Class{OS: "linux", Arch: "amd64", CNI: "aws-vpc"}

var want = corev1.NodeSelectorRequirement{Key: platform.CNILabel, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"aws-vpc"}}

// A site CNI DaemonSet with no affinity gets one required term excluding
// native-network nodes; nodes without the label still match it.
func TestASiteCNIIsKeptOffNativeNetworkNodes(t *testing.T) {
	e, c := excluder(t, calicoNode())
	if err := e.Ensure(context.Background(), get(t, c, "kube-system", "calico-node")); err != nil {
		t.Fatal(err)
	}
	terms := get(t, c, "kube-system", "calico-node").Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || !reflect.DeepEqual(terms[0].MatchExpressions, []corev1.NodeSelectorRequirement{want}) {
		t.Fatalf("terms = %+v", terms)
	}
	if err := e.Verify(context.Background(), cni.Network{Name: cni.Calico}, nativeClass); err != nil {
		t.Fatalf("verify after exclusion: %v", err)
	}
}

// Required terms are ORed, so the requirement goes into every one of them.
func TestTheRequirementIsAndedIntoEveryRequiredTerm(t *testing.T) {
	ds := calicoNode()
	ds.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
			{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "a", Operator: corev1.NodeSelectorOpExists}}},
			{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "b", Operator: corev1.NodeSelectorOpExists}}},
		}},
	}}
	e, c := excluder(t, ds)
	if err := e.Ensure(context.Background(), get(t, c, "kube-system", "calico-node")); err != nil {
		t.Fatal(err)
	}
	terms := get(t, c, "kube-system", "calico-node").Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	for i, key := range []string{"a", "b"} {
		got := terms[i].MatchExpressions
		if len(got) != 2 || got[0].Key != key || !reflect.DeepEqual(got[1], want) {
			t.Fatalf("term %d = %+v", i, got)
		}
	}
}

// Which DaemonSets are a site CNI: those that write the host CNI directories,
// Windows Calico by name (it mounts the whole host root), and any the
// operator names. Product components and ordinary node agents are not.
func TestTargetsAreTheDaemonSetsThatInstallAPodNetwork(t *testing.T) {
	windows := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "calico-node-windows", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{hostPathVolume("host", "/")}}}}}
	multus := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "kube-multus-ds", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{hostPathVolume("cni", "/etc/cni/net.d/")}}}}}
	proxy := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{hostPathVolume("modules", "/lib/modules")}}}}}
	named := proxy.DeepCopy()
	named.Name = "vendor-network-agent"
	owned := calicoNode()
	owned.Name = "aws-node"
	owned.Labels = map[string]string{platform.ComponentLabel: "aws-vpc"}
	e, _ := excluder(t, calicoNode(), windows, multus, proxy, named, owned)
	e.Extra = []client.ObjectKey{{Namespace: "kube-system", Name: "vendor-network-agent"}}
	targets, err := e.Targets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ds := range targets {
		names = append(names, ds.Name)
	}
	if !reflect.DeepEqual(names, []string{"calico-node", "calico-node-windows", "kube-multus-ds", "vendor-network-agent"}) {
		t.Fatalf("targets = %v", names)
	}
}

// Verification fails while any target can still schedule onto a native node.
func TestVerifyNamesEveryTargetNotYetExcluded(t *testing.T) {
	e, _ := excluder(t, calicoNode())
	err := e.Verify(context.Background(), cni.Network{Name: cni.Calico}, nativeClass)
	var pending *PendingError
	if !errors.As(err, &pending) || !reflect.DeepEqual(pending.DaemonSets, []string{"kube-system/calico-node"}) {
		t.Fatalf("verify = %v", err)
	}
}

// A network whose node agent cannot be scheduled away from a node has no
// exclusion the product can make.
func TestNetworksWithoutAnExcludableAgentAreUnsupported(t *testing.T) {
	for _, network := range []cni.Network{{Name: cni.OVNKubernetes}, {Name: cni.Flannel, Detail: "node annotation backend-type=vxlan"}, {Name: cni.Unrecognised}} {
		e, _ := excluder(t)
		var unsupported *UnsupportedError
		if err := e.Verify(context.Background(), network, nativeClass); !errors.As(err, &unsupported) {
			t.Fatalf("%s: verify = %v", network.Name, err)
		}
	}
}

// An operator-managed calico-node is changed through its Installation, which
// the operator renders from; editing the DaemonSet would be reverted.
func TestAnOperatorManagedCalicoIsExcludedThroughItsInstallation(t *testing.T) {
	ds := calicoNode()
	ds.Namespace = "calico-system"
	ds.OwnerReferences = []metav1.OwnerReference{{APIVersion: "operator.tigera.io/v1", Kind: "Installation", Name: "default", Controller: ptr(true)}}
	installation := &unstructured.Unstructured{}
	installation.SetGroupVersionKind(installationGVK)
	installation.SetName("default")
	_ = unstructured.SetNestedField(installation.Object, "Calico", "spec", "variant")
	e, c := excluder(t, ds, installation)
	if err := e.Ensure(context.Background(), get(t, c, "calico-system", "calico-node")); err != nil {
		t.Fatal(err)
	}
	if got := get(t, c, "calico-system", "calico-node"); got.Spec.Template.Spec.Affinity != nil {
		t.Fatal("the operator-owned DaemonSet was edited directly")
	}
	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(installationGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "default"}, updated); err != nil {
		t.Fatal(err)
	}
	terms, _, _ := unstructured.NestedSlice(updated.Object, "spec", "calicoNodeDaemonSet", "spec", "template", "spec",
		"affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms")
	if len(terms) != 1 {
		t.Fatalf("Installation override = %v", updated.Object["spec"])
	}
	if variant, _, _ := unstructured.NestedString(updated.Object, "spec", "variant"); variant != "Calico" {
		t.Fatal("the rest of the Installation was not preserved")
	}
}

// Another writer that keeps removing the exclusion stops this controller
// after three re-applies in ten minutes, and verification says why.
func TestAWriterThatKeepsRemovingTheExclusionTripsTheBreaker(t *testing.T) {
	e, c := excluder(t, calicoNode())
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	revert := func() {
		ds := get(t, c, "kube-system", "calico-node")
		ds.Spec.Template.Spec.Affinity = nil
		if err := c.Update(context.Background(), ds); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Ensure(context.Background(), get(t, c, "kube-system", "calico-node")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		revert()
		now = now.Add(time.Minute)
		if err := e.Ensure(context.Background(), get(t, c, "kube-system", "calico-node")); err != nil {
			t.Fatal(err)
		}
	}
	revert()
	now = now.Add(time.Minute)
	if err := e.Ensure(context.Background(), get(t, c, "kube-system", "calico-node")); err != nil {
		t.Fatal(err)
	}
	if get(t, c, "kube-system", "calico-node").Spec.Template.Spec.Affinity != nil {
		t.Fatal("re-applied after the breaker tripped")
	}
	var conflict *WriterConflictError
	if err := e.Verify(context.Background(), cni.Network{Name: cni.Calico}, nativeClass); !errors.As(err, &conflict) {
		t.Fatalf("verify = %v", err)
	}
	// The window passes: the breaker closes and the exclusion is restored.
	now = now.Add(11 * time.Minute)
	if err := e.Ensure(context.Background(), get(t, c, "kube-system", "calico-node")); err != nil {
		t.Fatal(err)
	}
	if get(t, c, "kube-system", "calico-node").Spec.Template.Spec.Affinity == nil {
		t.Fatal("not restored once the window passed")
	}
}

func ptr[T any](v T) *T { return &v }

// DaemonSets captured whole from real k0s sites are recognised and excluded.
// (The calico-node captures keep only the environment; calicoNode above
// carries the hostPath volumes of k0s's calico-node manifest.)
func TestCapturedK0sNetworkAgentsAreExcluded(t *testing.T) {
	for _, capture := range []string{
		"../testdata/k0s-v1.34.1-kube-router-v2.6.1/daemonset.json",
		"../testdata/k0s-v1.36.2-kube-router/daemonset.json",
	} {
		raw, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		ds := &appsv1.DaemonSet{}
		if err := json.Unmarshal(raw, ds); err != nil {
			t.Fatal(err)
		}
		e, c := excluder(t, ds)
		if !e.IsTarget(ds) {
			t.Fatalf("%s is not recognised as a pod network", capture)
		}
		if err := e.Ensure(context.Background(), get(t, c, ds.Namespace, ds.Name)); err != nil {
			t.Fatal(err)
		}
		if err := e.Verify(context.Background(), cni.Network{Name: cni.Calico}, nativeClass); err != nil {
			t.Fatalf("%s: %v", capture, err)
		}
	}
}

// The reconciler excludes a site network DaemonSet and ignores others.
func TestTheReconcilerExcludesOnlySiteNetworkAgents(t *testing.T) {
	proxy := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: "kube-system"}}
	e, c := excluder(t, calicoNode(), proxy)
	for _, name := range []string{"calico-node", "kube-proxy", "gone"} {
		if _, err := (Reconciler{e}).Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "kube-system", Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	if get(t, c, "kube-system", "calico-node").Spec.Template.Spec.Affinity == nil {
		t.Fatal("calico-node not excluded")
	}
	if get(t, c, "kube-system", "kube-proxy").Spec.Template.Spec.Affinity != nil {
		t.Fatal("kube-proxy was changed")
	}
}
