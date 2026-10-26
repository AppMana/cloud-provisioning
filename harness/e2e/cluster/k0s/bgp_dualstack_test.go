package k0s

import (
	"bytes"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"
	"text/template"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	native "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	"github.com/k0sproject/k0s/static"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/yaml"
)

func identityDeps(t *testing.T) cluster.Deps {
	t.Helper()
	topo, err := lab.WithIdentities(2)
	if err != nil {
		t.Fatal(err)
	}
	return cluster.Deps{Topology: topo, Network: BGPDualStack, K0sBinarySHA256: strings.Repeat("a", 64)}
}

func TestBGPDualStackReproducesTheDeployedNetwork(t *testing.T) {
	d := identityDeps(t)
	written, err := bgpDualStackConfig(d, d.Topology.MustNode("cp"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := asStored(written)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/bgp-dualstack-deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var want native.ClusterConfig
	if err := yaml.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]any{
		"network":           {got.Spec.Network, want.Spec.Network},
		"api":               {got.Spec.API, want.Spec.API},
		"storage":           {got.Spec.Storage, want.Spec.Storage},
		"controllerManager": {got.Spec.ControllerManager, want.Spec.ControllerManager},
		"telemetry":         {got.Spec.Telemetry, want.Spec.Telemetry},
	} {
		if !equality.Semantic.DeepEqual(pair[0], pair[1]) {
			g, _ := yaml.Marshal(pair[0])
			w, _ := yaml.Marshal(pair[1])
			t.Errorf("%s differs from the deployment\ngot:\n%s\nwant:\n%s", name, g, w)
		}
	}
	// Stored configuration is accepted on reuse only when it is this one.
	stored, err := yaml.Marshal(written)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyBGPDualStackConfig(stored, d, d.Topology.MustNode("cp")); err != nil {
		t.Fatal(err)
	}
	written.Spec.Network.Calico.Overlay = "Always"
	changed, _ := yaml.Marshal(written)
	if verifyBGPDualStackConfig(changed, d, d.Topology.MustNode("cp")) == nil {
		t.Fatal("reuse accepted a different Calico overlay")
	}
}

func TestBGPDualStackInstallsLikeTheDeployment(t *testing.T) {
	d := identityDeps(t)
	cp, w1 := d.Topology.MustNode("cp"), d.Topology.MustNode("w1")
	if got, want := bgpDualStackControllerArgs(cp), []string{"--enable-worker", "--no-taints",
		"--disable-components=konnectivity-server,metrics-server",
		"--kubelet-extra-args=--node-ip=10.101.0.1,fd8f:cf26:522a::1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("controller args %q", got)
	}
	if got, want := bgpDualStackWorkerArgs(w1), []string{
		"--kubelet-extra-args=--node-ip=10.101.0.2,fd8f:cf26:522a::2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("worker args %q", got)
	}
	if networkProvider(BGPDualStack) != "calico" {
		t.Fatal("provider")
	}
}

// The profile refuses anything but one controller on an identity site.
func TestBGPDualStackRefusesOtherSites(t *testing.T) {
	d := cluster.Deps{Topology: lab.Default(), Network: BGPDualStack, K0sBinarySHA256: strings.Repeat("a", 64)}
	if _, err := bgpDualStackConfig(d, d.Topology.MustNode("cp")); err == nil {
		t.Fatal("accepted three controllers on LAN addresses")
	}
}

func TestBGPDualStackImagesAreTheStockReleasePinned(t *testing.T) {
	images := bgpDualStackImages()
	for name, image := range map[string]*native.ImageSpec{
		"calico.cni": images.Calico.CNI, "calico.node": images.Calico.Node, "calico.kubecontrollers": images.Calico.KubeControllers,
		"kubeproxy": images.KubeProxy, "coredns": images.CoreDNS, "pause": images.Pause,
	} {
		if !strings.HasPrefix(image.Image, "quay.io/k0sproject/") || !pinnedVersion.MatchString(image.Version) {
			t.Fatalf("%s is %s:%s", name, image.Image, image.Version)
		}
	}
	if errs := images.Validate(nil); len(errs) != 0 {
		t.Fatal(errs.ToAggregate())
	}
	if err := ValidateImages(BGPDualStack, &native.ClusterImages{}); err == nil {
		t.Fatal("accepted a caller image set for the stock profile")
	}
}

// The profile reproduces the deployment exactly; a required change is
// applied on top of it explicitly and nowhere else. A pod MTU over the
// tunnel's is that change: lowering it is k0s's own spec.network.calico.mtu,
// and every other field stays the deployment's.
func TestBGPDualStackTakesARequiredPodMTUAndNothingElse(t *testing.T) {
	d := identityDeps(t)
	base, err := bgpDualStackConfig(d, d.Topology.MustNode("cp"))
	if err != nil {
		t.Fatal(err)
	}
	d.K0sCalicoMTU = 1420
	lowered, err := bgpDualStackConfig(d, d.Topology.MustNode("cp"))
	if err != nil {
		t.Fatal(err)
	}
	if lowered.Spec.Network.Calico.MTU != 1420 {
		t.Fatalf("calico MTU = %d, want 1420", lowered.Spec.Network.Calico.MTU)
	}
	lowered.Spec.Network.Calico.MTU = base.Spec.Network.Calico.MTU
	if !equality.Semantic.DeepEqual(base, lowered) {
		t.Fatal("the MTU change altered other deployment fields")
	}
	// Reuse accepts the stored configuration only with the same MTU.
	raw, err := yaml.Marshal(func() *native.ClusterConfig { c, _ := bgpDualStackConfig(d, d.Topology.MustNode("cp")); return c }())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyBGPDualStackConfig(raw, d, d.Topology.MustNode("cp")); err != nil {
		t.Fatal(err)
	}
	d.K0sCalicoMTU = 0
	if verifyBGPDualStackConfig(raw, d, d.Topology.MustNode("cp")) == nil {
		t.Fatal("reuse accepted a site whose MTU differs from the requested one")
	}
}

// The builder validates a site's configuration before touching any node,
// and that validation must accept what the profile itself accepts.
func TestTheBuilderAcceptsTheDualStackProfilesRequiredMTU(t *testing.T) {
	d := identityDeps(t)
	d.K0sCalicoMTU = 1420
	if err := validateSiteConfig(d); err != nil {
		t.Fatalf("validateSiteConfig: %v", err)
	}
	d.K0sCalicoManagedAddresses = true
	if err := validateSiteConfig(d); err == nil {
		t.Fatal("managed addresses would change the deployment's own autodetection, and were accepted")
	}
}

func TestDualStackSiteOwnershipIsExplicitAndVerifiedOnReuse(t *testing.T) {
	d := identityDeps(t)
	d.K0sCalicoSiteOnly = true
	config, err := bgpDualStackConfig(d, d.Topology.MustNode("cp"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySiteBGPConfig(raw); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"IPV4", "IPV6"} {
		if config.Spec.Network.Calico.EnvVars["CALICO_"+family+"POOL_NODE_SELECTOR"] == "" {
			t.Fatal("unbounded IPAM")
		}
	}
	for _, n := range d.Topology.NodesInRole(lab.Worker) {
		if !strings.Contains(kubeletNodeIPs(n, true), siteCalicoLabel+"="+siteCalicoValue) {
			t.Fatal("site label missing before registration")
		}
	}
	d.K0sCalicoSiteOnly = false
	if verifyBGPDualStackConfig(raw, d, d.Topology.MustNode("cp")) == nil {
		t.Fatal("reuse ignored CNI ownership")
	}
}

// Use the pinned distribution's actual interpolation fragment: serializing the
// ClusterConfig alone did not catch selector quotes breaking the generated DS.
func TestSiteSelectorsRenderThroughK0sEnvironmentTemplate(t *testing.T) {
	d := identityDeps(t)
	d.K0sCalicoSiteOnly = true
	config, err := bgpDualStackConfig(d, d.Topology.MustNode("cp"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := fs.ReadFile(static.CalicoManifests, "DaemonSet/calico-node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(source), "{{ range $name, $value := .EnvVars }}")
	if start < 0 {
		t.Fatal("k0s environment template changed")
	}
	end := strings.Index(string(source[start:]), "{{- end }}")
	if end < 0 {
		t.Fatal("k0s environment template changed")
	}
	tmpl, err := template.New("k0s-env").Parse(string(source[start : start+end+len("{{- end }}")]))
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, map[string]any{"EnvVars": config.Spec.Network.Calico.EnvVars}); err != nil {
		t.Fatal(err)
	}
	var vars []corev1.EnvVar
	if err := yaml.Unmarshal(rendered.Bytes(), &vars); err != nil {
		t.Fatalf("k0s generated invalid environment YAML: %v", err)
	}
	found := 0
	for _, v := range vars {
		if strings.HasPrefix(v.Name, "CALICO_IPV") && strings.HasSuffix(v.Name, "POOL_NODE_SELECTOR") {
			found++
		}
	}
	if found != 2 {
		t.Fatal("both family selectors must render")
	}
}
