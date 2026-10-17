package k0s

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"sigs.k8s.io/yaml"
)

// installedNode answers what a reused site node reports: its k0s version,
// the digest of its installed binary and its stored configuration.
type installedNode struct {
	rig.Node
	digest  string
	version string
	config  []byte
}

func (n installedNode) Exec(_ context.Context, args ...string) ([]byte, error) {
	switch strings.Join(args, " ") {
	case "k0s version":
		if n.version != "" {
			return []byte(n.version + "\n"), nil
		}
		return []byte(Version + "\n"), nil
	case "sha256sum /usr/local/bin/k0s":
		return []byte(n.digest + "  /usr/local/bin/k0s\n"), nil
	case "cat /etc/k0s/k0s.yaml":
		return n.config, nil
	}
	return nil, nil
}

type installedRig struct {
	rig.Rig
	nodes map[string]installedNode
}

func (r installedRig) Node(name string) rig.Node { return r.nodes[name] }

// A command that reuses a site, such as an AWS row, is not given the
// binary the site was built from; the digest installed on the site's nodes
// is that pin, and every node must carry the same one.
func TestReuseTakesTheBinaryPinFromTheSiteNodes(t *testing.T) {
	d := identityDeps(t)
	d.K0sCalicoMTU = 1420
	config, err := bgpDualStackConfig(d, d.Topology.MustNode("cp"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	installed := strings.Repeat("b", 64)
	nodes := map[string]installedNode{}
	for _, n := range d.Topology.Nodes {
		nodes[n.Name] = installedNode{digest: installed, config: raw}
	}
	d.Rig = installedRig{nodes: nodes}
	d.Kube = &kube.Client{Bastion: &roleBastion{}, ControlPlanes: []string{"10.101.0.1"}}
	d.K0sBinarySHA256 = ""
	if err := (Builder{}).Reuse(context.Background(), d); err != nil {
		t.Fatalf("reuse without a given pin: %v", err)
	}
	d.K0sBinarySHA256 = installed
	if err := (Builder{}).Reuse(context.Background(), d); err != nil {
		t.Fatalf("reuse with the installed pin: %v", err)
	}
	d.K0sBinarySHA256 = strings.Repeat("c", 64)
	if err := (Builder{}).Reuse(context.Background(), d); err == nil {
		t.Fatal("reuse accepted a site whose binary differs from the given pin")
	}
	d.K0sBinarySHA256 = ""
	w := nodes["w1"]
	w.digest = strings.Repeat("d", 64)
	nodes["w1"] = w
	if err := (Builder{}).Reuse(context.Background(), d); err == nil || !strings.Contains(err.Error(), "w1") {
		t.Fatalf("reuse of a site with mixed binaries: %v", err)
	}
}

// This verifies artifact admission with simulated guests, not runtime health,
// embedded component provenance, boot recovery or forwarding continuity.
func TestReuseForkRequiresMatchingArtifactIdentity(t *testing.T) {
	d := identityDeps(t)
	d.K0sSourceRevision = strings.Repeat("a", 40)
	d.K0sVersion = Version + ".appmana." + d.K0sSourceRevision[:12]
	config, err := bgpDualStackConfig(d, d.Topology.MustNode("cp"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]installedNode{}
	for _, n := range d.Topology.Nodes {
		nodes[n.Name] = installedNode{digest: d.K0sBinarySHA256, version: d.K0sVersion, config: raw}
	}
	d.Rig = installedRig{nodes: nodes}
	d.Kube = &kube.Client{Bastion: &roleBastion{}, ControlPlanes: []string{"10.101.0.1"}}
	if err := (Builder{}).Reuse(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []string{"missing-digest", "wrong-digest", "wrong-source", "missing-source", "wrong-version", "unsupported-kubernetes"} {
		t.Run(tc, func(t *testing.T) {
			bad := d
			switch tc {
			case "missing-digest":
				bad.K0sBinarySHA256 = ""
			case "wrong-digest":
				bad.K0sBinarySHA256 = strings.Repeat("b", 64)
			case "wrong-source":
				bad.K0sSourceRevision = strings.Repeat("b", 40)
			case "missing-source":
				bad.K0sSourceRevision = ""
			case "wrong-version":
				bad.K0sVersion = Version
			case "unsupported-kubernetes":
				bad.K0sVersion = "v1.37.0+k0s.1"
			}
			if err := (Builder{}).Reuse(context.Background(), bad); err == nil {
				t.Fatal("accepted mismatched artifact identity")
			}
		})
	}
}
