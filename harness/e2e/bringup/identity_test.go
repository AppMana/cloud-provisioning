package bringup

import (
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"sigs.k8s.io/yaml"
)

// The identity file is the deployment's own: a dummy vip0 holding both
// identities, and the IPv4 identity prefix on-link on the LAN link.
func TestIdentityNetplanMatchesTheDeployment(t *testing.T) {
	topo, err := lab.WithIdentities(2)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := identityNetplan(topo.MustNode("w1"), "ens2", topo.IdentityRoutes)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := yaml.Unmarshal(doc, &got); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(`
network:
  version: 2
  renderer: networkd
  dummy-devices:
    vip0:
      addresses: ["10.101.0.2/32", "fd8f:cf26:522a::2/128"]
  ethernets:
    ens2:
      routes:
        - to: 10.101.0.0/24
          scope: link
`), &want); err != nil {
		t.Fatal(err)
	}
	if !equalJSON(got, want) {
		t.Fatalf("got\n%s", doc)
	}
}

func equalJSON(a, b any) bool {
	x, _ := yaml.Marshal(a)
	y, _ := yaml.Marshal(b)
	return string(x) == string(y)
}
