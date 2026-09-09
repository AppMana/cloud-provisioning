package k0s

import (
	"context"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"sigs.k8s.io/yaml"
)

type configNode struct {
	rig.Node
	config []byte
}

func (n *configNode) Exec(context.Context, ...string) ([]byte, error) { return nil, nil }
func (n *configNode) Put(_ context.Context, src io.Reader, _ string, _ fs.FileMode) error {
	var err error
	n.config, err = io.ReadAll(src)
	return err
}

type configRig struct {
	rig.Rig
	node *configNode
}

func (r configRig) Node(string) rig.Node { return r.node }

func TestRenderedK0sProviderMatchesNativeDefault(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		mtu        int
	}{
		{"", "kuberouter", 0}, {"default", "kuberouter", 0}, {"kube-router", "kuberouter", 0}, {"kuberouter", "kuberouter", 0}, {"calico", "calico", 1370}, {"custom", "custom", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &configNode{}
			d := cluster.Deps{Topology: lab.Default(), Rig: configRig{node: n}, PodCIDR: "10.244.0.0/16", SvcCIDR: "10.96.0.0/12", Network: tc.name, K0sCalicoMTU: tc.mtu}
			if err := (Builder{}).config(context.Background(), d, d.Topology.MustNode("cp")); err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				Spec struct {
					Network struct {
						Provider string `json:"provider"`
					} `json:"network"`
				} `json:"spec"`
			}
			if err := yaml.Unmarshal(n.config, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Spec.Network.Provider != tc.want {
				t.Fatalf("provider=%s, want %s", cfg.Spec.Network.Provider, tc.want)
			}
			if tc.want != "calico" && strings.Contains(string(n.config), "calico:") {
				t.Fatal("injected Calico configuration into another provider")
			}
		})
	}
}
