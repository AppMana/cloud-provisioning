package site

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

var tunnelClass = platform.Class{OS: "linux", Arch: "amd64", CNI: platform.SiteCNI}

// k0s's own calico-node, rendered from its bundled manifest (testdata/README.md).
func k0sCalicoNode(t *testing.T) *appsv1.DaemonSet {
	t.Helper()
	raw, err := os.ReadFile("testdata/k0s-v1.36.5-calico-node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ds := &appsv1.DaemonSet{}
	if err := yaml.Unmarshal(raw, ds); err != nil {
		t.Fatal(err)
	}
	return ds
}

func env(c corev1.Container) map[string]string {
	vars := map[string]string{}
	for _, e := range c.Env {
		vars[e.Name] = e.Value
	}
	return vars
}

func container(spec corev1.PodSpec, name string) corev1.Container {
	for _, c := range spec.Containers {
		if c.Name == name {
			return c
		}
	}
	return corev1.Container{}
}

func bgpConfiguration(c client.Client, t *testing.T) *unstructured.Unstructured {
	t.Helper()
	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(bgpConfigurationGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "default"}, config); err != nil {
		t.Fatal(err)
	}
	return config
}

func bgpPeer(name, nodeSelector, peerSelector, node string) *unstructured.Unstructured {
	peer := &unstructured.Unstructured{}
	peer.SetGroupVersionKind(bgpPeerGVK)
	peer.SetName(name)
	spec := map[string]any{}
	for key, value := range map[string]string{"nodeSelector": nodeSelector, "peerSelector": peerSelector, "node": node, "peerIP": "10.10.0.1"} {
		if value != "" {
			spec[key] = value
		}
	}
	peer.Object["spec"] = spec
	return peer
}

func meshDisabled(on bool) *unstructured.Unstructured {
	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(bgpConfigurationGVK)
	config.SetName("default")
	_ = unstructured.SetNestedField(config.Object, !on, "spec", "nodeToNodeMeshEnabled")
	return config
}

// On a tunnel-class cloud node the site's Calico runs as the product's own
// agent: the same pod, scheduled only on tunnel-class cloud workers, with
// address detection off so the tunnel address the product states is the
// one Calico keeps. The site's calico-node stays off every cloud worker,
// and the site's BGP mesh is limited to site nodes, since the tunnel
// carries no BGP and a peer that never establishes makes Calico unready.
func TestTunnelNodesRunTheSiteCalicoAsTheProductsAgent(t *testing.T) {
	ctx := context.Background()
	e, c := excluder(t, k0sCalicoNode(t))
	if err := e.Verify(ctx, cni.Network{Name: cni.Calico}, tunnelClass); err == nil {
		t.Fatal("a tunnel node was admitted before the product's Calico agent existed")
	}
	if err := e.Ensure(ctx, get(t, c, "kube-system", "calico-node")); err != nil {
		t.Fatal(err)
	}

	siteDS := get(t, c, "kube-system", "calico-node")
	role := corev1.NodeSelectorRequirement{Key: platform.RoleLabel, Operator: corev1.NodeSelectorOpNotIn, Values: []string{platform.RoleValue}}
	for _, term := range siteDS.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		if !slices.ContainsFunc(term.MatchExpressions, func(r corev1.NodeSelectorRequirement) bool { return reflect.DeepEqual(r, role) }) {
			t.Fatalf("the site's calico-node can still run on cloud workers: %+v", term)
		}
	}

	agent := get(t, c, "kube-system", "calico-node-tunnel")
	if agent.Labels[platform.ComponentLabel] == "" {
		t.Fatal("the agent is not marked as a product component")
	}
	if len(agent.OwnerReferences) != 1 || agent.OwnerReferences[0].Name != "calico-node" || agent.OwnerReferences[0].Kind != "DaemonSet" {
		t.Fatalf("owner = %+v", agent.OwnerReferences)
	}
	selector, err := metav1.LabelSelectorAsSelector(siteDS.Spec.Selector)
	if err != nil {
		t.Fatal(err)
	}
	if selector.Matches(labels.Set(agent.Spec.Template.Labels)) {
		t.Fatal("the site's calico-node selects the agent's pods")
	}
	terms := agent.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	wantTerm := []corev1.NodeSelectorRequirement{
		{Key: platform.RoleLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{platform.RoleValue}},
		{Key: platform.CNILabel, Operator: corev1.NodeSelectorOpDoesNotExist},
	}
	if len(terms) != 1 || !reflect.DeepEqual(terms[0].MatchExpressions, wantTerm) {
		t.Fatalf("agent placement = %+v", terms)
	}
	siteNode, node := container(siteDS.Spec.Template.Spec, "calico-node"), container(agent.Spec.Template.Spec, "calico-node")
	if node.Image != siteNode.Image || !reflect.DeepEqual(agent.Spec.Template.Spec.Volumes, siteDS.Spec.Template.Spec.Volumes) ||
		!reflect.DeepEqual(agent.Spec.Template.Spec.InitContainers, siteDS.Spec.Template.Spec.InitContainers) {
		t.Fatal("the agent does not run the site's Calico")
	}
	vars := env(node)
	if vars["IP"] != "none" || vars["IP6"] != "none" {
		t.Fatalf("address detection still on: IP=%q IP6=%q", vars["IP"], vars["IP6"])
	}
	if _, ok := vars["IP_AUTODETECTION_METHOD"]; ok {
		t.Fatal("the agent carries an autodetection method it never uses")
	}
	if vars["CALICO_NETWORKING_BACKEND"] != env(siteNode)["CALICO_NETWORKING_BACKEND"] {
		t.Fatal("the agent changed the site's backend")
	}

	if mesh, _, _ := unstructured.NestedBool(bgpConfiguration(c, t).Object, "spec", "nodeToNodeMeshEnabled"); mesh {
		t.Fatal("the full mesh still peers every node with every cloud node")
	}
	peer := &unstructured.Unstructured{}
	peer.SetGroupVersionKind(bgpPeerGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: siteMeshPeer}, peer); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"nodeSelector", "peerSelector"} {
		if got, _, _ := unstructured.NestedString(peer.Object, "spec", field); got != "!has("+platform.RoleLabel+")" {
			t.Fatalf("site mesh %s = %q", field, got)
		}
	}
	if err := e.Verify(ctx, cni.Network{Name: cni.Calico}, tunnelClass); err != nil {
		t.Fatalf("verify after ensuring: %v", err)
	}
}

// The agent follows the site's Calico: an upgrade of the site's DaemonSet is
// an upgrade of the agent, and a hand edit of the agent is undone.
func TestTheTunnelAgentFollowsTheSiteCalico(t *testing.T) {
	ctx := context.Background()
	e, c := excluder(t, k0sCalicoNode(t))
	if err := e.Ensure(ctx, get(t, c, "kube-system", "calico-node")); err != nil {
		t.Fatal(err)
	}
	upgraded := get(t, c, "kube-system", "calico-node")
	upgraded.Spec.Template.Spec.Containers[0].Image = "quay.io/k0sproject/calico-node:v3.32.2-0"
	if err := c.Update(ctx, upgraded); err != nil {
		t.Fatal(err)
	}
	edited := get(t, c, "kube-system", "calico-node-tunnel")
	edited.Spec.Template.Spec.Containers[0].Env = nil
	if err := c.Update(ctx, edited); err != nil {
		t.Fatal(err)
	}
	if err := e.Ensure(ctx, get(t, c, "kube-system", "calico-node")); err != nil {
		t.Fatal(err)
	}
	node := container(get(t, c, "kube-system", "calico-node-tunnel").Spec.Template.Spec, "calico-node")
	if node.Image != "quay.io/k0sproject/calico-node:v3.32.2-0" || env(node)["IP"] != "none" {
		t.Fatalf("agent image %q IP %q", node.Image, env(node)["IP"])
	}
}

// Peering the site already scopes is left alone. A peer that can reach a
// cloud node is the operator's to change, so a tunnel node is refused with
// the peer named; one that provably excludes cloud nodes is fine.
func TestBGPPeersThatReachCloudNodesRefuseTunnelNodes(t *testing.T) {
	ctx := context.Background()
	remote := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "remote1", Labels: map[string]string{platform.RoleLabel: platform.RoleValue}}}
	site := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}}
	for _, tc := range []struct {
		peer *unstructured.Unstructured
		ok   bool
	}{
		{bgpPeer("tor", "!has("+platform.RoleLabel+") && rack == 'a'", "", ""), true},
		{bgpPeer("w1-tor", "", "", "w1"), true},
		{bgpPeer("everyone", "all()", "", ""), false},
		{bgpPeer("global", "", "", ""), false},
		{bgpPeer("remote-tor", "", "", "remote1"), false},
		{bgpPeer("reflectors", "!has("+platform.RoleLabel+")", "has(route-reflector)", ""), false},
	} {
		e, c := excluder(t, k0sCalicoNode(t), remote, site, meshDisabled(true), tc.peer)
		if err := e.Ensure(ctx, get(t, c, "kube-system", "calico-node")); err != nil {
			t.Fatal(err)
		}
		if mesh, _, _ := unstructured.NestedBool(bgpConfiguration(c, t).Object, "spec", "nodeToNodeMeshEnabled"); mesh {
			t.Fatal("the site's own peering choice was changed")
		}
		err := e.Verify(ctx, cni.Network{Name: cni.Calico}, tunnelClass)
		var unsupported *UnsupportedError
		if tc.ok && err != nil || !tc.ok && !errors.As(err, &unsupported) {
			t.Errorf("peer %s: verify = %v", tc.peer.GetName(), err)
		}
		if !tc.ok && err != nil && !strings.Contains(err.Error(), tc.peer.GetName()) {
			t.Errorf("refusal does not name peer %s: %v", tc.peer.GetName(), err)
		}
	}
}

// Only Calico needs this; a tunnel node on another site network joins it as
// the site runs it.
func TestOtherSiteNetworksAdmitTunnelNodesAsTheyAre(t *testing.T) {
	e, _ := excluder(t)
	if err := e.Verify(context.Background(), cni.Network{Name: cni.KubeRouter}, tunnelClass); err != nil {
		t.Fatal(err)
	}
}

// Under the Tigera operator the exclusion takes effect when the operator
// renders it into calico-node, and only then does the agent start, so the
// two never run on one node.
func TestTheTunnelAgentWaitsForTheOperatorToExcludeCloudWorkers(t *testing.T) {
	ctx := context.Background()
	ds := k0sCalicoNode(t)
	ds.Namespace = "calico-system"
	ds.OwnerReferences = []metav1.OwnerReference{{APIVersion: "operator.tigera.io/v1", Kind: "Installation", Name: "default", Controller: ptr(true)}}
	installation := &unstructured.Unstructured{}
	installation.SetGroupVersionKind(installationGVK)
	installation.SetName("default")
	e, c := excluder(t, ds, installation)
	if err := e.Ensure(ctx, get(t, c, "calico-system", "calico-node")); err != nil {
		t.Fatal(err)
	}
	agent := &appsv1.DaemonSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "calico-system", Name: "calico-node-tunnel"}, agent); err == nil {
		t.Fatal("the agent started before the operator excluded cloud workers from calico-node")
	}
	// The operator renders the Installation's affinity into calico-node.
	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(installationGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: "default"}, updated); err != nil {
		t.Fatal(err)
	}
	affinity, _, _ := unstructured.NestedMap(updated.Object, "spec", "calicoNodeDaemonSet", "spec", "template", "spec", "affinity")
	rendered := get(t, c, "calico-system", "calico-node")
	rendered.Spec.Template.Spec.Affinity = &corev1.Affinity{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(affinity, rendered.Spec.Template.Spec.Affinity); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, rendered); err != nil {
		t.Fatal(err)
	}
	if err := e.Ensure(ctx, get(t, c, "calico-system", "calico-node")); err != nil {
		t.Fatal(err)
	}
	get(t, c, "calico-system", "calico-node-tunnel")
}

// k0s's own calico-node-windows, rendered from its bundled manifest (testdata/README.md).
func k0sCalicoNodeWindows(t *testing.T) *appsv1.DaemonSet {
	t.Helper()
	raw, err := os.ReadFile("testdata/k0s-v1.36.5-calico-node-windows.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ds := &appsv1.DaemonSet{}
	if err := yaml.Unmarshal(raw, ds); err != nil {
		t.Fatal(err)
	}
	return ds
}

// Windows tunnel-class workers get the same treatment from the site's
// calico-node-windows: k0s sets its IP to the autodetection method, which
// detects the address on every check, so the site's agent stays off cloud
// workers and the product's copy runs on Windows tunnel-class ones with
// detection off in both of its Calico containers.
func TestWindowsTunnelNodesRunTheSiteWindowsCalicoAsTheProductsAgent(t *testing.T) {
	ctx := context.Background()
	e, c := excluder(t, k0sCalicoNode(t), k0sCalicoNodeWindows(t))
	for _, name := range []string{"calico-node", "calico-node-windows"} {
		if err := e.Ensure(ctx, get(t, c, "kube-system", name)); err != nil {
			t.Fatal(err)
		}
	}
	siteDS := get(t, c, "kube-system", "calico-node-windows")
	role := corev1.NodeSelectorRequirement{Key: platform.RoleLabel, Operator: corev1.NodeSelectorOpNotIn, Values: []string{platform.RoleValue}}
	for _, term := range siteDS.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		if !slices.ContainsFunc(term.MatchExpressions, func(r corev1.NodeSelectorRequirement) bool { return reflect.DeepEqual(r, role) }) {
			t.Fatalf("the site's calico-node-windows can still run on cloud workers: %+v", term)
		}
	}
	agent := get(t, c, "kube-system", "calico-node-windows-tunnel")
	linux := get(t, c, "kube-system", "calico-node-tunnel")
	if reflect.DeepEqual(agent.Spec.Selector, linux.Spec.Selector) {
		t.Fatalf("the Windows and Linux agents share a selector: %+v", agent.Spec.Selector)
	}
	for _, name := range []string{"felix", "node"} {
		vars := env(container(agent.Spec.Template.Spec, name))
		if vars["IP"] != "none" || vars["IP6"] != "none" {
			t.Fatalf("%s still detects its address: %v", name, vars)
		}
		if _, ok := vars["IP_AUTODETECTION_METHOD"]; ok {
			t.Fatalf("%s keeps an autodetection method", name)
		}
	}
	if agent.Spec.Template.Spec.NodeSelector[corev1.LabelOSStable] != "windows" {
		t.Fatalf("node selector %v", agent.Spec.Template.Spec.NodeSelector)
	}
	if err := e.Verify(ctx, cni.Network{Name: cni.Calico}, platform.Class{OS: "windows", Arch: "amd64", CNI: platform.SiteCNI}); err != nil && !errors.As(err, new(*PendingError)) {
		t.Fatal(err)
	}
	if err := e.verifyTunnelAgent(ctx, siteDS); err != nil {
		t.Fatalf("the Windows agent is not current: %v", err)
	}
}

func TestBGPScopeCompletesBeforeProviderPools(t *testing.T) {
	ctx := context.Background()
	e, c := excluder(t, k0sCalicoNode(t))
	e.ProviderRanges = func(context.Context) ([]netip.Prefix, error) {
		return nil, errors.New("provider pool discovery pending")
	}
	err := e.Ensure(ctx, get(t, c, "kube-system", "calico-node"))
	if err == nil {
		t.Fatal("expected provider pool error")
	}
	if err := e.verifyBGPScope(ctx); err != nil {
		t.Fatalf("BGP mesh scope was blocked by provider pool discovery: %v", err)
	}
}

// When k0s leaves install-cni's CNI_NETWORK_CONFIG at "mtu": 1450 while
// FELIX_*MTU is 1500 (or calico-windows-config-actual drifts on CNI_MTU / IP),
// Excluder.Ensure reconciles both the site Calico and the Windows BGP ConfigMap
// before copying the pod template into calico-node-tunnel.
func TestEnsureAutomaticallyAlignsCalicoMTUAndWindowsBGPConfigMap(t *testing.T) {
	ctx := context.Background()
	ds := k0sCalicoNode(t)
	for i := range ds.Spec.Template.Spec.Containers {
		if ds.Spec.Template.Spec.Containers[i].Name != "calico-node" {
			continue
		}
		for j := range ds.Spec.Template.Spec.Containers[i].Env {
			switch ds.Spec.Template.Spec.Containers[i].Env[j].Name {
			case "FELIX_IPINIPMTU", "FELIX_VXLANMTU", "FELIX_WIREGUARDMTU":
				ds.Spec.Template.Spec.Containers[i].Env[j].Value = "1500"
			}
		}
	}
	winCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "calico-windows-config-actual"},
		Data: map[string]string{
			"CALICO_NETWORKING_BACKEND": "windows-bgp",
			"IP_AUTODETECTION_METHOD":   "cidr=192.168.10.0/24",
			"CNI_MTU":                   "1450",
			"FELIX_MTU":                 "1450",
		},
	}
	winDS := k0sCalicoNodeWindows(t)
	e, c := excluder(t, ds, winCM, winDS)
	for _, name := range []string{"calico-node", "calico-node-windows"} {
		if err := e.Ensure(ctx, get(t, c, "kube-system", name)); err != nil {
			t.Fatal(err)
		}
	}

	for _, dsName := range []string{"calico-node", "calico-node-tunnel"} {
		gotDS := get(t, c, "kube-system", dsName)
		changed, effMTU, _, err := AlignLinuxCalicoMTU(gotDS.DeepCopy(), 0)
		if err != nil {
			t.Fatalf("%s: AlignLinuxCalicoMTU: %v", dsName, err)
		}
		if changed || effMTU != 1500 {
			t.Fatalf("%s: expected already-aligned effectiveMTU=1500, got changed=%v effectiveMTU=%d", dsName, changed, effMTU)
		}
	}

	var gotCM corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "calico-windows-config-actual"}, &gotCM); err != nil {
		t.Fatal(err)
	}
	if gotCM.Data["CNI_MTU"] != "1500" || gotCM.Data["FELIX_MTU"] != "1500" || gotCM.Data["IP"] != "autodetect" || gotCM.Data["CALICO_DSR_DISABLE"] != "true" {
		t.Fatalf("unexpected calico-windows-config-actual data: %+v", gotCM.Data)
	}
}
