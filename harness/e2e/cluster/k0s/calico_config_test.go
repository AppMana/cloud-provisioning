package k0s

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	native "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/yaml"
)

func TestStoredCalicoAddressesPatchObservedDaemonSet(t *testing.T) {
	for _, network := range []string{"", "default", "kuberouter", "kube-router", "custom", "cilium"} {
		if _, err := calicoConfig(network, 0, true); err == nil {
			t.Fatalf("injected Calico address configuration into %s", network)
		}
	}
	// Projection of the actual k0s 1.36.2 Calico DaemonSet: environment,
	// image and rollout strategy. The canary removed IP=autodetect because
	// native logs showed its minute-based monitor overwriting stored addresses.
	original, err := os.ReadFile("testdata/calico-node-environment.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, mtu := range []int{0, 1370} {
		config, err := calicoConfig("calico", mtu, true)
		if err != nil {
			t.Fatal(err)
		}
		if config.MTU != mtu || len(config.Patches) != 1 {
			t.Fatal("missing configuration")
		}
		patch := config.Patches[0]
		if patch.Target != (native.PatchTarget{Kind: "DaemonSet", Name: "calico-node", Namespace: "kube-system"}) || patch.Patch.Type != native.StrategicMergePatchType {
			t.Fatal("wrong target or patch type")
		}
		updated, err := strategicpatch.StrategicMergePatch(original, []byte(patch.Patch.Content), appsv1.DaemonSet{})
		if err != nil {
			t.Fatal(err)
		}
		var before, after appsv1.DaemonSet
		if err := json.Unmarshal(original, &before); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(updated, &after); err != nil {
			t.Fatal(err)
		}
		wanted := before.DeepCopy()
		env := wanted.Spec.Template.Spec.Containers[0].Env[:0]
		found := false
		for _, item := range before.Spec.Template.Spec.Containers[0].Env {
			if item.Name == "IP" {
				found = true
				continue
			}
			env = append(env, item)
		}
		wanted.Spec.Template.Spec.Containers[0].Env = env
		if !found || !reflect.DeepEqual(wanted.Spec, after.Spec) {
			t.Fatal("patch did not exclusively remove autodetection from the observed spec")
		}
	}
}

func TestCalicoMTUOverridePreservesProfileBoundaries(t *testing.T) {
	for _, network := range []string{"default", "calico", "kuberouter", "custom"} {
		config, err := calicoConfig(network, 0, false)
		if err != nil || config != nil {
			t.Fatalf("zero override changed distro defaults for %s: %+v, %v", network, config, err)
		}
	}
	for _, network := range []string{"", "default", "kuberouter", "kube-router", "custom", "cilium"} {
		if _, err := calicoConfig(network, 1370, false); err == nil {
			t.Fatalf("injected Calico configuration into %s", network)
		}
	}
	for _, mtu := range []int{-1, 1279, 65536} {
		if _, err := calicoConfig("calico", mtu, false); err == nil {
			t.Fatalf("accepted invalid MTU %d", mtu)
		}
	}
	for _, network := range []string{"calico"} {
		config, err := calicoConfig(network, 1370, false)
		if err != nil {
			t.Fatal(err)
		}
		if config.MTU != 1370 {
			t.Fatalf("wrong observed path budget: %+v", config)
		}
	}
}

// Apply the real strategic patch before checking scheduling: replacing the
// selector would discard k0s's Linux constraint and expose Windows hosts.
func TestSiteBGPPatchRestrictsInstallerAndPool(t *testing.T) {
	original, err := os.ReadFile("testdata/calico-node-environment.json")
	if err != nil {
		t.Fatal(err)
	}
	var base appsv1.DaemonSet
	if err := json.Unmarshal(original, &base); err != nil {
		t.Fatal(err)
	}
	base.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/os": "linux"}
	original, _ = json.Marshal(base)
	for _, managed := range []bool{false, true} {
		config, err := calicoConfig("calico-site-bgp", 1370, managed)
		if err != nil {
			t.Fatal(err)
		}
		if config.Mode != native.CalicoModeBIRD || config.Overlay != "Never" || len(config.Patches) != 1 {
			t.Fatalf("invalid native BGP configuration: %+v", config)
		}
		raw, err := strategicpatch.StrategicMergePatch(original, []byte(config.Patches[0].Patch.Content), appsv1.DaemonSet{})
		if err != nil {
			t.Fatal(err)
		}
		var after appsv1.DaemonSet
		if err := json.Unmarshal(raw, &after); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"kubernetes.io/os": "linux", siteCalicoLabel: siteCalicoValue}
		if !reflect.DeepEqual(after.Spec.Template.Spec.NodeSelector, want) {
			t.Fatalf("installer can escape site Linux nodes: %+v", after.Spec.Template.Spec.NodeSelector)
		}
		found := false
		for _, e := range after.Spec.Template.Spec.Containers[0].Env {
			if managed && e.Name == "IP" {
				t.Fatal("lost stored-address override")
			}
			if e.Name == "CALICO_IPV4POOL_NODE_SELECTOR" {
				found = e.Value == siteCalicoLabel+` == "calico-site"`
			}
		}
		if !found {
			t.Fatal("pool can allocate addresses outside the site")
		}
		if !reflect.DeepEqual(base.Spec.Template.Spec.InitContainers, after.Spec.Template.Spec.InitContainers) {
			t.Fatal("changed the distro CNI installer")
		}
	}
}

func TestSiteBGPReuseRejectsOverlayAndUnscopedCalico(t *testing.T) {
	for _, network := range []string{"calico", "calico-site-bgp"} {
		cfg, err := calicoConfig(network, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := yaml.Marshal(&native.ClusterConfig{Spec: &native.ClusterSpec{Network: &native.Network{Provider: "calico", Calico: cfg}}})
		if err != nil {
			t.Fatal(err)
		}
		err = verifySiteBGPConfig(raw)
		if (err == nil) != (network == "calico-site-bgp") {
			t.Fatalf("%s reuse: %v", network, err)
		}
	}
	raw, err := yaml.Marshal(&native.ClusterConfig{Spec: &native.ClusterSpec{Network: &native.Network{
		Provider: "calico", Calico: &native.Calico{Mode: native.CalicoModeBIRD, Overlay: "Never"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if verifySiteBGPConfig(raw) == nil {
		t.Fatal("accepted unscoped Calico")
	}
}
