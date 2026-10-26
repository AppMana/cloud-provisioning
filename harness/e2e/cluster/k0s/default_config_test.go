package k0s

import (
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"sigs.k8s.io/yaml"
)

// renderedConfig is the native configuration a controller is installed with.
func renderedConfig(t *testing.T, d cluster.Deps) []byte {
	t.Helper()
	config, err := (Builder{}).config(d, d.Topology.MustNode("cp"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestRenderedK0sProviderMatchesNativeDefault(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		mtu        int
	}{
		{"", "kuberouter", 0}, {"default", "kuberouter", 0}, {"kube-router", "kuberouter", 0}, {"kuberouter", "kuberouter", 0}, {"calico", "calico", 1370}, {"custom", "custom", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := cluster.Deps{Topology: lab.Default(), PodCIDR: "10.244.0.0/16", SvcCIDR: "10.96.0.0/12", Network: tc.name, K0sCalicoMTU: tc.mtu}
			if tc.want == "calico" {
				d.K0sImages = pinnedTestImages()
			}
			rendered := renderedConfig(t, d)
			var cfg struct {
				Spec struct {
					Network struct {
						Provider string `json:"provider"`
					} `json:"network"`
				} `json:"spec"`
			}
			if err := yaml.Unmarshal(rendered, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Spec.Network.Provider != tc.want {
				t.Fatalf("provider=%s, want %s", cfg.Spec.Network.Provider, tc.want)
			}
			if tc.want != "calico" && strings.Contains(string(rendered), "calico:") {
				t.Fatal("injected Calico configuration into another provider")
			}
		})
	}
}
