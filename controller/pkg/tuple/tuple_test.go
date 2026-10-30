package tuple

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni/site"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func sampleWindowsOptions() WindowsBGPOptions {
	return WindowsBGPOptions{
		NodeImage:               "ghcr.io/appmana/node@sha256:" + strings.Repeat("a", 64),
		APIHost:                 "192.168.10.10",
		APIPort:                 "6443",
		ServiceCIDR:             "10.96.0.0/12",
		DNSAddress:              "10.96.0.10",
		AutodetectionMethod:     "cidr=192.168.10.0/24",
		IPv6AutodetectionMethod: "cidr=fd5a:8000:1::/64",
		MTU:                     1500,
		ExcludeCloudWorkers:     true,
		ExcludeNativeCNIs:       []string{"aws-vpc"},
	}
}

func TestParseAndValidatePlatformTuples(t *testing.T) {
	valid := []struct {
		raw       string
		wantCanon string
		wantCNI   string
		wantOS    string
		wantCloud string
	}{
		{"k0s + calico,bgp + aws", "k0s + calico,bgp + aws", "calico", "mixed", "aws-vpc"},
		{"k0s+calico,bird,windows+aws,aws-vpc", "k0s + calico,bgp,windows + aws", "calico", "windows", "aws-vpc"},
		{"k0s + calico,bgp + aws,site", "k0s + calico,bgp + aws,site", "calico", "mixed", "site"},
		{"k0s + cilium,linux + aws", "k0s + cilium,linux + aws", "cilium", "linux", "aws-vpc"},
	}
	for _, tc := range valid {
		got, err := Parse(tc.raw)
		if err != nil {
			t.Fatalf("Parse(%q) unexpected error: %v", tc.raw, err)
		}
		if got.String() != tc.wantCanon || got.CNI != tc.wantCNI || got.GuestOS != tc.wantOS || got.CloudCNI != tc.wantCloud {
			t.Fatalf("Parse(%q) = %+v (%q), want canon=%q cni=%q os=%q cloud=%q", tc.raw, got, got.String(), tc.wantCanon, tc.wantCNI, tc.wantOS, tc.wantCloud)
		}
	}

	invalid := []string{
		"k0s + calico",
		"k0s + calico,vxlan + aws",
		"k0s + cilium + aws",                  // mixed includes windows, cilium does not support windows
		"kubeadm + calico,bgp,windows + aws",  // windows requires k0s
		"k0s + flannel,windows + aws,site",    // flannel does not support windows
		"k0s + calico,bgp + docker,aws-vpc",   // aws-vpc requires aws provider
		"unknown + calico,bgp + aws",
	}
	for _, raw := range invalid {
		if got, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) succeeded with %+v, want error", raw, got)
		}
	}
}

func TestRenderWindowsBGPAndK0sNetwork(t *testing.T) {
	opts := sampleWindowsOptions()
	plan, err := NewPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Objects()) != 3 {
		t.Fatalf("expected 3 objects, got %d", len(plan.Objects()))
	}
	manifestStr := string(plan.Manifest())
	for _, want := range []string{
		`"name":"calico-windows-config-actual"`,
		`"CALICO_NETWORKING_BACKEND":"windows-bgp"`,
		`"CALICO_DSR_DISABLE":"true"`,
		`"CNI_MTU":"1500"`,
		`"FELIX_IPINIPMTU":"1500"`,
		`"strictAffinity":true`,
		`"autoAllocateBlocks":true`,
		`Test-CalicoBGPNodeReady`,
		`"cloud-provisioning.appmana.com/role"`,
		`"cloud-provisioning.appmana.com/cni"`,
	} {
		if !strings.Contains(manifestStr, want) {
			t.Fatalf("rendered manifest missing %s", want)
		}
	}

	netSpec, err := K0sCalicoNetwork(K0sNetworkOptions{
		MTU:                     1500,
		AutodetectionMethod:     "cidr=192.168.10.0/24",
		IPv6AutodetectionMethod: "cidr=fd5a:8000:1::/64",
		PodCIDR:                 "10.244.0.0/16",
		ServiceCIDR:             "10.96.0.0/12",
		IPv6PodCIDR:             "fd00:10:244::/56",
		IPv6ServiceCIDR:         "fd00:10:96::/108",
	})
	if err != nil {
		t.Fatal(err)
	}
	rawCfg, err := json.Marshal(map[string]interface{}{"spec": netSpec})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateK0sCalicoConfig(rawCfg); err != nil {
		t.Fatalf("ValidateK0sCalicoConfig failed on rendered spec: %v", err)
	}
}

func loadCapturedCluster(t *testing.T) client.Client {
	t.Helper()
	dir := filepath.Join("..", "cni", "testdata", "k0s-v1.36.4-calico-v3.32.1-bird-dualstack")
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	for _, kind := range []string{"IPPool", "BlockAffinity", "IPAMBlock", "BGPPeer", "BGPConfiguration", "IPAMConfig"} {
		gvk := schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: kind}
		sc.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		sc.AddKnownTypeWithName(gvk.GroupVersion().WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	var objs []client.Object

	var ds appsv1.DaemonSet
	mustUnmarshalFile(t, filepath.Join(dir, "calico-node.json"), &ds)
	objs = append(objs, &ds)

	var nodes corev1.NodeList
	mustUnmarshalFile(t, filepath.Join(dir, "nodes.json"), &nodes)
	for i := range nodes.Items {
		objs = append(objs, &nodes.Items[i])
	}

	for _, file := range []string{"ippools.json", "blocks.json"} {
		list := &unstructured.UnstructuredList{}
		mustUnmarshalFile(t, filepath.Join(dir, file), list)
		for i := range list.Items {
			objs = append(objs, &list.Items[i])
		}
	}
	return fake.NewClientBuilder().WithScheme(sc).WithObjects(objs...).Build()
}

func mustUnmarshalFile(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func setLinuxCalicoMTU(t *testing.T, ctx context.Context, c client.Client, cniMTU, felixMTU int) {
	t.Helper()
	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: "calico-node"}, &ds); err != nil {
		t.Fatal(err)
	}
	for i := range ds.Spec.Template.Spec.InitContainers {
		for j := range ds.Spec.Template.Spec.InitContainers[i].Env {
			env := &ds.Spec.Template.Spec.InitContainers[i].Env[j]
			if env.Name == "CNI_NETWORK_CONFIG" {
				env.Value = strings.ReplaceAll(env.Value, `"mtu": 1450`, `"mtu": `+itoa(cniMTU))
				env.Value = strings.ReplaceAll(env.Value, `"mtu": 1500`, `"mtu": `+itoa(cniMTU))
				env.Value = strings.ReplaceAll(env.Value, `"mtu": 1420`, `"mtu": `+itoa(cniMTU))
			}
		}
	}
	for i := range ds.Spec.Template.Spec.Containers {
		if ds.Spec.Template.Spec.Containers[i].Name != "calico-node" {
			continue
		}
		for j := range ds.Spec.Template.Spec.Containers[i].Env {
			env := &ds.Spec.Template.Spec.Containers[i].Env[j]
			switch env.Name {
			case "FELIX_IPINIPMTU", "FELIX_VXLANMTU", "FELIX_WIREGUARDMTU":
				env.Value = itoa(felixMTU)
			}
		}
	}
	if err := c.Update(ctx, &ds); err != nil {
		t.Fatal(err)
	}
}

func itoa(v int) string {
	return json.Number(strings.TrimSpace(string(mustJSON(v)))).String()
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func byName(results []CheckResult) map[string]CheckResult {
	m := map[string]CheckResult{}
	for _, r := range results {
		m[r.Name] = r
	}
	return m
}

func TestEvaluateCatchesMTUDriftMissingWindowsBGPAndKonnectivityEgress(t *testing.T) {
	ctx := context.Background()
	c := loadCapturedCluster(t)

	// 1. Captured cluster has CNI_NETWORK_CONFIG mtu=1450 and FELIX_IPINIPMTU=1500.
	// Evaluate with RequireWindowsBGP=true must catch both calico-mtu drift and missing windows-bgp.
	results, err := Evaluate(ctx, c, CheckOptions{
		Tuple:             "k0s + calico,bgp + aws",
		RequireDualStack:  true,
		RequireWindowsBGP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := byName(results)
	if !got["tuple"].Pass || !got["network"].Pass || !got["dual-stack"].Pass {
		t.Fatalf("expected tuple/network/dual-stack to pass: %+v", got)
	}
	if got["calico-mtu"].Pass || !strings.Contains(got["calico-mtu"].Detail, "disagrees with FELIX_IPINIPMTU=1500") {
		t.Fatalf("expected calico-mtu to catch 1450 vs 1500 drift, got: %+v", got["calico-mtu"])
	}
	if got["windows-bgp"].Pass {
		t.Fatalf("expected windows-bgp to fail before WindowsBGP resources are installed: %+v", got["windows-bgp"])
	}

	// 2. Add konnectivity-agent without konnectivity-agent-default-route-linux:
	// k0s-windows-egress must fail.
	if err := c.Create(ctx, &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "konnectivity-agent"},
	}); err != nil {
		t.Fatal(err)
	}
	results, err = Evaluate(ctx, c, CheckOptions{
		Tuple:             "k0s + calico,bgp + aws",
		RequireDualStack:  true,
		RequireWindowsBGP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got = byName(results)
	if got["k0s-windows-egress"].Pass {
		t.Fatalf("expected k0s-windows-egress to fail when konnectivity-agent-default-route-linux is missing: %+v", got["k0s-windows-egress"])
	}

	// 3. Fix Linux Calico MTU to 1500, install rendered WindowsBGP objects, and
	// create konnectivity-agent-default-route-linux: all checks pass for k0s + calico,bgp + aws!
	setLinuxCalicoMTU(t, ctx, c, 1500, 1500)
	objs, err := WindowsBGP(sampleWindowsOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objs {
		if err := c.Create(ctx, obj.(client.Object)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Create(ctx, &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "konnectivity-agent-default-route-linux"},
	}); err != nil {
		t.Fatal(err)
	}

	results, err = Evaluate(ctx, c, CheckOptions{
		Tuple:             "k0s + calico,bgp + aws",
		RequireDualStack:  true,
		RequireWindowsBGP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !Passed(results) {
		t.Fatalf("expected all tuple checks to pass after remediation, got: %+v", results)
	}

	// 4. Verify VerifyClaimEnvironment succeeds for both Linux and Windows AWS VPC claims.
	detected, err := cni.Detect(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	ex := &site.Excluder{Client: c, Reader: c, Native: []string{"aws-vpc"}, Now: time.Now}
	targets, err := ex.Targets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range targets {
		if err := ex.Ensure(ctx, &targets[i]); err != nil {
			t.Fatal(err)
		}
	}
	for _, osName := range []string{"linux", "windows"} {
		class := platform.Class{OS: osName, CNI: "aws-vpc"}
		if err := VerifyClaimEnvironment(ctx, c, "k0s", detected, class, ex); err != nil {
			t.Fatalf("VerifyClaimEnvironment(%s) failed: %v", osName, err)
		}
	}
}

func TestEvaluateCatchesAutodetectionWindowsBGPAndPeerPitfalls(t *testing.T) {
	ctx := context.Background()
	c := loadCapturedCluster(t)
	setLinuxCalicoMTU(t, ctx, c, 1420, 1420)

	// 1. Overwrite IP_AUTODETECTION_METHOD on calico-node to overlap TunnelIPv4 (10.100.0.0/24).
	var linuxDS appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: "calico-node"}, &linuxDS); err != nil {
		t.Fatal(err)
	}
	for i := range linuxDS.Spec.Template.Spec.Containers {
		if linuxDS.Spec.Template.Spec.Containers[i].Name != "calico-node" {
			continue
		}
		for j := range linuxDS.Spec.Template.Spec.Containers[i].Env {
			if linuxDS.Spec.Template.Spec.Containers[i].Env[j].Name == "IP_AUTODETECTION_METHOD" {
				linuxDS.Spec.Template.Spec.Containers[i].Env[j].Value = "cidr=10.100.0.0/16"
			}
		}
	}
	if err := c.Update(ctx, &linuxDS); err != nil {
		t.Fatal(err)
	}
	results, err := Evaluate(ctx, c, CheckOptions{Tuple: "k0s + calico,bgp + aws"})
	if err != nil {
		t.Fatal(err)
	}
	got := byName(results)
	if got["calico-autodetection"].Pass || !strings.Contains(got["calico-autodetection"].Detail, "overlaps tunnel subnet") {
		t.Fatalf("expected calico-autodetection overlap failure, got: %+v", got["calico-autodetection"])
	}

	// 2. Restore IP_AUTODETECTION_METHOD, add a BGPPeer without !has(cloud-provisioning.appmana.com/role),
	// and check that cloudCNI=site rejects it while cloudCNI=aws-vpc allows it (unless StrictBGPPeerScope=true).
	for i := range linuxDS.Spec.Template.Spec.Containers {
		if linuxDS.Spec.Template.Spec.Containers[i].Name != "calico-node" {
			continue
		}
		for j := range linuxDS.Spec.Template.Spec.Containers[i].Env {
			if linuxDS.Spec.Template.Spec.Containers[i].Env[j].Name == "IP_AUTODETECTION_METHOD" {
				linuxDS.Spec.Template.Spec.Containers[i].Env[j].Value = "cidr=192.168.10.0/24"
			}
		}
	}
	if err := c.Update(ctx, &linuxDS); err != nil {
		t.Fatal(err)
	}
	peer := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "crd.projectcalico.org/v1",
		"kind":       "BGPPeer",
		"metadata":   map[string]interface{}{"name": "vyos-ipv4"},
		"spec":       map[string]interface{}{"peerIP": "192.168.10.1", "asNumber": int64(65000)},
	}}
	if err := c.Create(ctx, peer); err != nil {
		t.Fatal(err)
	}
	siteRes, err := Evaluate(ctx, c, CheckOptions{Tuple: "k0s + calico,bgp + aws,site"})
	if err != nil {
		t.Fatal(err)
	}
	if byName(siteRes)["bgp-peers"].Pass {
		t.Fatalf("expected bgp-peers to fail for cloudCNI=site with unscoped BGPPeer: %+v", byName(siteRes)["bgp-peers"])
	}
	awsVPCRes, err := Evaluate(ctx, c, CheckOptions{Tuple: "k0s + calico,bgp + aws"})
	if err != nil {
		t.Fatal(err)
	}
	if !byName(awsVPCRes)["bgp-peers"].Pass {
		t.Fatalf("expected bgp-peers to pass for cloudCNI=aws-vpc when StrictBGPPeerScope=false: %+v", byName(awsVPCRes)["bgp-peers"])
	}

	// 3. Install WindowsBGP objects and corrupt calico-windows-config-actual (missing IP=autodetect)
	// and verify windows-bgp catches it.
	winOpts := sampleWindowsOptions()
	winOpts.MTU = 1420
	objs, err := WindowsBGP(winOpts)
	if err != nil {
		t.Fatal(err)
	}
	cm := objs[0].(*corev1.ConfigMap)
	delete(cm.Data, "IP")
	for _, obj := range objs {
		if err := c.Create(ctx, obj.(client.Object)); err != nil {
			t.Fatal(err)
		}
	}
	winRes, err := Evaluate(ctx, c, CheckOptions{Tuple: "k0s + calico,bgp + aws", RequireWindowsBGP: true})
	if err != nil {
		t.Fatal(err)
	}
	if byName(winRes)["windows-bgp"].Pass || !strings.Contains(byName(winRes)["windows-bgp"].Detail, "IP=") {
		t.Fatalf("expected windows-bgp to catch missing IP=autodetect, got: %+v", byName(winRes)["windows-bgp"])
	}

	// 4. Reintroduce the 1450 vs 1500 MTU mismatch on calico-node, then run
	// ReconcileCalicoSite (shared by cmd/platform-tuple --mode=fix and site.Excluder.Ensure)
	// and verify both Linux Calico MTU and Windows BGP ConfigMap are healed automatically.
	setLinuxCalicoMTU(t, ctx, c, 1450, 1500)
	report, err := ReconcileCalicoSite(ctx, c, c, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !report.LinuxDaemonSetPatched || !report.WindowsConfigPatched || report.EffectiveMTU != 1500 {
		t.Fatalf("expected ReconcileCalicoSite to heal MTU to 1500, got: %+v", report)
	}
	healedRes, err := Evaluate(ctx, c, CheckOptions{Tuple: "k0s + calico,bgp + aws", RequireWindowsBGP: true})
	if err != nil {
		t.Fatal(err)
	}
	if !byName(healedRes)["calico-mtu"].Pass || !byName(healedRes)["windows-bgp"].Pass {
		t.Fatalf("expected calico-mtu and windows-bgp to pass after ReconcileCalicoSite, got: calico-mtu=%+v windows-bgp=%+v",
			byName(healedRes)["calico-mtu"], byName(healedRes)["windows-bgp"])
	}
}
