package transitguard

import (
	"encoding/json"
	"net"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func fixture(t *testing.T) Plan {
	t.Helper()
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("requires an isolated Linux network namespace")
	}
	lan := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "tg-lan"}}
	if err := netlink.LinkAdd(lan); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netlink.LinkDel(lan) })
	if err := netlink.LinkSetUp(lan); err != nil {
		t.Fatal(err)
	}
	c := Config{Interface: "retired-wg", SourceTable: 517, ShadowTable: 1000517}
	t.Cleanup(func() {
		all, err := rules()
		if err != nil {
			t.Error(err)
			return
		}
		for i := range all {
			if all[i].Table == c.ShadowTable {
				if err := netlink.RuleDel(&all[i]); err != nil {
					t.Error(err)
				}
			}
		}
	})
	for _, tc := range []struct {
		address, gateway, destination string
		family                        int
	}{
		{"192.0.2.2/24", "192.0.2.1", "198.51.100.3/32", netlink.FAMILY_V4},
		{"2001:db8:1::2/64", "2001:db8:1::1", "2001:db8:2::3/128", netlink.FAMILY_V6},
	} {
		addr, _ := netlink.ParseAddr(tc.address)
		addr.Flags = unix.IFA_F_NODAD
		if err := netlink.AddrAdd(lan, addr); err != nil {
			t.Fatal(err)
		}
		_, dst, _ := net.ParseCIDR(tc.destination)
		route := netlink.Route{LinkIndex: lan.Attrs().Index, Dst: dst, Gw: net.ParseIP(tc.gateway), Src: addr.IP, Table: c.SourceTable, Type: unix.RTN_UNICAST, Priority: 17, Protocol: 99}
		if err := netlink.RouteAdd(&route); err != nil {
			t.Fatal(err)
		}
		rule := netlink.NewRule()
		rule.Table = c.SourceTable
		rule.Priority = c.SourceTable
		rule.Family = tc.family
		if err := netlink.RuleAdd(rule); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { netlink.RuleDel(rule) })
	}
	p, err := Observe(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func reachable(t *testing.T, p Plan) {
	t.Helper()
	for _, want := range p.Routes {
		got, err := netlink.RouteGet(want.Dst.IP)
		if err != nil || len(got) != 1 || got[0].LinkIndex != want.LinkIndex || !got[0].Gw.Equal(want.Gw) || !got[0].Src.Equal(want.Src) {
			t.Fatalf("destination %s lookup %+v: %v", want.Dst, got, err)
		}
	}
}

func TestTransitSurvivesLegacyRuleRemoval(t *testing.T) {
	p := fixture(t)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restored Plan
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	p = restored
	reachable(t, p)
	if err := Install(p); err != nil {
		t.Fatal(err)
	}
	reachable(t, p)
	// The legacy shutdown deletes every rule selecting the source table.
	for i := range p.Rules {
		if err := netlink.RuleDel(&p.Rules[i]); err != nil {
			t.Fatal(err)
		}
	}
	reachable(t, p)
	if err := Remove(p); err == nil {
		t.Fatal("withdrew fallback before primary routing returned")
	}
	reachable(t, p)
	for i := range p.Rules {
		if err := netlink.RuleAdd(&p.Rules[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := Remove(p); err != nil {
		t.Fatal(err)
	}
	reachable(t, p)
	left, err := routes(p.Config.ShadowTable)
	if err != nil || len(left) != 0 {
		t.Fatalf("leftover routes: %+v %v", left, err)
	}
}

func TestShadowCollisionDoesNotModifyExistingRouting(t *testing.T) {
	p := fixture(t)
	foreign := netlink.NewRule()
	foreign.Table = 99999
	foreign.Priority = p.Config.SourceTable + 1
	foreign.Family = netlink.FAMILY_V4
	if err := netlink.RuleAdd(foreign); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netlink.RuleDel(foreign) })
	if err := Install(p); err == nil {
		t.Fatal("accepted occupied priority")
	}
	reachable(t, p)
	got, err := routes(p.Config.ShadowTable)
	if err != nil || len(got) != 0 {
		t.Fatalf("modified shadow table: %+v %v", got, err)
	}
	all, err := rules()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range all {
		if r.Priority == foreign.Priority && r.Table == foreign.Table {
			found = true
		}
	}
	if !found {
		t.Fatal("removed another owner's rule")
	}
}

func TestSourceDriftRejectsInstallation(t *testing.T) {
	p := fixture(t)
	changed := p.Routes[0]
	changed.Gw = net.ParseIP("192.0.2.9")
	if err := netlink.RouteReplace(&changed); err != nil {
		t.Fatal(err)
	}
	if err := Install(p); err == nil {
		t.Fatal("accepted stale plan")
	}
	got, err := routes(p.Config.ShadowTable)
	if err != nil || len(got) != 0 {
		t.Fatalf("modified shadow table: %+v %v", got, err)
	}
}

func TestOwnedTunnelRejectsTransitGuard(t *testing.T) {
	p := fixture(t)
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: p.Config.Interface}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netlink.LinkDel(link) })
	if err := Install(p); err == nil {
		t.Fatal("accepted node with an owned tunnel")
	}
}

func TestCleanupResumesAfterPartialWithdrawal(t *testing.T) {
	p := fixture(t)
	if err := Install(p); err != nil {
		t.Fatal(err)
	}
	rs, qs := shadow(p)
	if err := netlink.RuleDel(&qs[0]); err != nil {
		t.Fatal(err)
	}
	if err := netlink.RouteDel(&rs[0]); err != nil {
		t.Fatal(err)
	}
	if err := Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := Remove(p); err != nil {
		t.Fatal(err)
	}
	reachable(t, p)
}

func TestCleanupPreservesChangedShadowState(t *testing.T) {
	p := fixture(t)
	if err := Install(p); err != nil {
		t.Fatal(err)
	}
	rs, _ := shadow(p)
	changed := rs[0]
	changed.MTU = 1300
	if err := netlink.RouteReplace(&changed); err != nil {
		t.Fatal(err)
	}
	if err := Remove(p); err == nil {
		t.Fatal("removed another writer's changed route")
	}
	all, err := routes(p.Config.ShadowTable)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range all {
		if r.MTU == 1300 {
			found = true
		}
	}
	if !found {
		t.Fatal("changed route was lost")
	}
}

func TestNonLookupSourceRuleIsRejected(t *testing.T) {
	p := fixture(t)
	if err := netlink.RuleDel(&p.Rules[0]); err != nil {
		t.Fatal(err)
	}
	r := p.Rules[0]
	r.Type = unix.RTN_BLACKHOLE
	if err := netlink.RuleAdd(&r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netlink.RuleDel(&r) })
	if _, err := Observe(p.Config); err == nil {
		t.Fatal("blackhole action appeared as lookup")
	}
}

func TestGotoTargetRejectsInstallation(t *testing.T) {
	p := fixture(t)
	r := netlink.NewRule()
	r.Family = netlink.FAMILY_V4
	r.Priority = 500
	r.Goto = p.Config.SourceTable + 1
	if err := netlink.RuleAdd(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netlink.RuleDel(r) })
	if err := Install(p); err == nil {
		t.Fatal("changed routing for an existing goto target")
	}
	got, err := routes(p.Config.ShadowTable)
	if err != nil || len(got) != 0 {
		t.Fatalf("modified shadow table: %+v %v", got, err)
	}
}

func TestOccupiedShadowTableIsPreserved(t *testing.T) {
	p := fixture(t)
	foreign := p.Routes[0]
	foreign.Table = p.Config.ShadowTable
	if err := netlink.RouteAdd(&foreign); err != nil {
		t.Fatal(err)
	}
	if err := Install(p); err == nil {
		t.Fatal("accepted another owner's route")
	}
	got, err := routes(p.Config.ShadowTable)
	if err != nil || len(got) != 1 || !sameRoutes(got, []netlink.Route{foreign}) {
		t.Fatalf("changed foreign route: %+v %v", got, err)
	}
}
