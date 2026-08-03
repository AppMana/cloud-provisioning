package tunnel

import (
	"strings"
	"testing"
)

func TestHostCIDR(t *testing.T) {
	cases := map[string]string{
		"10.100.0.2":        "10.100.0.2/32",
		"fd8f:cf26:522a::1": "fd8f:cf26:522a::1/128",
		"10.100.0.2/32":     "10.100.0.2/32", // already has a prefix -- left alone
		"10.100.0.0/24":     "10.100.0.0/24", // never narrowed if already broader (rejection is the route parser's job)
	}
	for in, want := range cases {
		if got := HostCIDR(in); got != want {
			t.Errorf("HostCIDR(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitList(t *testing.T) {
	got := SplitList("10.244.0.0/16, ", "10.96.0.0/12,fd00::/108", "")
	want := []string{"10.244.0.0/16", "10.96.0.0/12", "fd00::/108"}
	if len(got) != len(want) {
		t.Fatalf("SplitList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SplitList[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestInterfaceName(t *testing.T) {
	name := InterfaceName("wg-dialer/wg-dialer-peer")
	if !strings.HasPrefix(name, "cldt") {
		t.Errorf("InterfaceName = %q, want cldt prefix", name)
	}
	if len(name) != 12 {
		t.Errorf("InterfaceName = %q (len %d), want 12 chars (under IFNAMSIZ 15)", name, len(name))
	}
	if name == "wg0" || strings.HasPrefix(name, "wg") {
		t.Errorf("InterfaceName = %q must never collide with the wgX namespace", name)
	}
	if InterfaceName("wg-dialer/wg-dialer-peer") != name {
		t.Error("InterfaceName is not deterministic")
	}
	if InterfaceName("other/mesh") == name {
		t.Error("InterfaceName does not vary with mesh identity")
	}
}

func TestAllRouteHosts_FoldsLegacyField(t *testing.T) {
	p := PeerSpec{RouteHosts: []string{"10.100.0.2", "10.101.0.4"}, RouteHost: "fd8f:cf26:522a::4"}
	got := p.AllRouteHosts()
	if len(got) != 3 {
		t.Fatalf("AllRouteHosts = %v, want 3 entries", got)
	}
}
