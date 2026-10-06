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

// IPv6 neighbour discovery is strong-host: a node answers a solicitation
// only for an address on the interface it arrived on, so an identity on
// vip0 is unreachable across the LAN even inside the LAN's own /64. The
// IPv4 identities are reached by weak-host ARP; the IPv6 ones need the
// node to answer for them on its LAN link by proxy. Without it Calico's
// IPv6 sessions, which run between the identities it selects, never
// establish and the site has no IPv6 pod routes at all.
func TestIPv6IdentitiesAnswerNeighbourDiscoveryOnTheLAN(t *testing.T) {
	topo, err := lab.WithIdentities(2)
	if err != nil {
		t.Fatal(err)
	}
	path, doc := identityProxyNDP(topo.MustNode("w1"), "ens2")
	if path != "/etc/systemd/network/10-netplan-ens2.network.d/70-cluster-vip-ndp.conf" {
		t.Errorf("drop-in path %s does not extend netplan's ens2 network", path)
	}
	if want := "[Network]\nIPv6ProxyNDP=yes\nIPv6ProxyNDPAddress=fd8f:cf26:522a::2\n"; string(doc) != want {
		t.Errorf("drop-in =\n%s\nwant\n%s", doc, want)
	}
	if path, doc := identityProxyNDP(topo.MustNode("bastion"), "eth1"); path != "" || doc != nil {
		t.Errorf("a node without identities got %s", path)
	}
}
