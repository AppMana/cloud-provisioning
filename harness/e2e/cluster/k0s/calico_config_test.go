package k0s

import (
	"testing"

	native "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	"sigs.k8s.io/yaml"
)

func TestCalicoMTUOverridePreservesProfileBoundaries(t *testing.T) {
	for _, network := range []string{"default", "calico", "kuberouter", "custom"} {
		config, err := calicoConfig(network, 0)
		if err != nil || config != nil {
			t.Fatalf("zero override changed distro defaults for %s: %+v, %v", network, config, err)
		}
	}
	for _, network := range []string{"", "default", "kuberouter", "kube-router", "custom", "cilium"} {
		if _, err := calicoConfig(network, 1370); err == nil {
			t.Fatalf("injected Calico configuration into %s", network)
		}
	}
	for _, mtu := range []int{-1, 1279, 65536} {
		if _, err := calicoConfig("calico", mtu); err == nil {
			t.Fatalf("accepted invalid MTU %d", mtu)
		}
	}
	for _, network := range []string{"calico"} {
		config, err := calicoConfig(network, 1370)
		if err != nil {
			t.Fatal(err)
		}
		if config.MTU != 1370 {
			t.Fatalf("wrong observed path budget: %+v", config)
		}
	}
}

// The site BGP profile configures BGP routing only. Which nodes run calico-node,
// and which address Calico keeps on a cloud node, are left to the product, so
// the site stays the unmodified deployment an operator has.
func TestSiteBGPLeavesCalicoPlacementAndAddressesToTheProduct(t *testing.T) {
	config, err := calicoConfig("calico-site-bgp", 1370)
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != native.CalicoModeBIRD || config.Overlay != "Never" {
		t.Fatalf("invalid native BGP configuration: %+v", config)
	}
	if len(config.Patches) != 0 {
		t.Fatalf("the site patches its own Calico: %+v", config.Patches)
	}
	if args := siteKubeletArgs("192.0.2.1"); args != "--kubelet-extra-args=--node-ip=192.0.2.1" {
		t.Fatalf("site kubelet arguments = %q", args)
	}
}

func TestSiteBGPReuseRejectsOverlay(t *testing.T) {
	for _, network := range []string{"calico", "calico-site-bgp"} {
		cfg, err := calicoConfig(network, 0)
		if err != nil {
			t.Fatal(err)
		}
		if cfg == nil {
			cfg = &native.Calico{Mode: native.CalicoModeVXLAN}
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
}
