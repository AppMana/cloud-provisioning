// Package transitguard preserves a transit-only node's routes across an agent
// upgrade. The caller must journal the plan, freeze routing writers, and retain
// it until cleanup succeeds. This is not a deployment authorization gate.
package transitguard

import (
	"errors"
	"fmt"
	"net"
	"reflect"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

type Config struct {
	Interface   string `json:"interface"`
	SourceTable int    `json:"sourceTable"`
	ShadowTable int    `json:"shadowTable"`
}

type Plan struct {
	Config Config          `json:"config"`
	Routes []netlink.Route `json:"routes"`
	Rules  []netlink.Rule  `json:"rules"`
}

func validate(c Config) error {
	if c.Interface == "" || len(c.Interface) > 15 || c.SourceTable <= 255 || c.SourceTable >= 32765 || c.ShadowTable <= 255 || c.ShadowTable > 2147483647 || c.SourceTable == c.ShadowTable || c.ShadowTable == c.SourceTable+65536 {
		return errors.New("explicit transit interface and distinct reserved routing tables required")
	}
	if _, err := netlink.LinkByName(c.Interface); err == nil {
		return errors.New("owned tunnel still exists; only transit-only upgrades are supported")
	} else {
		var absent netlink.LinkNotFoundError
		if !errors.As(err, &absent) {
			return err
		}
	}
	return nil
}

func routes(table int) ([]netlink.Route, error) {
	var out []netlink.Route
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		found, err := netlink.RouteListFiltered(family, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

func rules() ([]netlink.Rule, error) {
	var out []netlink.Rule
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		found, err := netlink.RuleList(family)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

// Observe captures only nondefault, unicast routes through a relay and ordinary
// source-table lookup rules. Unsupported policy shapes fail before mutation.
func Observe(c Config) (Plan, error) {
	p := Plan{Config: c}
	if err := validate(c); err != nil {
		return p, err
	}
	rs, err := routes(c.SourceTable)
	if err != nil {
		return p, err
	}
	if len(rs) == 0 {
		return p, errors.New("no transit routes to preserve")
	}
	for _, r := range rs {
		if r.Dst == nil || r.Gw == nil || r.LinkIndex <= 0 || r.Type != unix.RTN_UNICAST || len(r.MultiPath) != 0 || r.Encap != nil || r.Via != nil || r.NewDst != nil {
			return p, fmt.Errorf("unsupported transit route: %s", r.String())
		}
		ones, _ := r.Dst.Mask.Size()
		if ones <= 0 {
			return p, errors.New("default transit route is unsupported")
		}
	}
	all, err := rules()
	if err != nil {
		return p, err
	}
	for _, r := range all {
		if r.Table != c.SourceTable {
			continue
		}
		expected := netlink.NewRule()
		expected.Family = r.Family
		expected.Table = c.SourceTable
		expected.Priority = c.SourceTable
		expected.Protocol = r.Protocol
		if !reflect.DeepEqual(r, *expected) {
			return p, errors.New("source table has unsupported policy rule")
		}
		p.Rules = append(p.Rules, r)
	}
	if err := lookupActions(c.SourceTable, len(p.Rules)); err != nil {
		return p, err
	}
	if len(p.Rules) != 2 || p.Rules[0].Family == p.Rules[1].Family {
		return p, errors.New("one source lookup rule per IP family required")
	}
	p.Routes = rs
	return p, nil
}

// netlink v1.3.1 RuleList omits the fib rule action. Check the raw
// action separately rather than treating a blackhole as a lookup rule.
func lookupActions(table, expected int) error {
	count := 0
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		req := nl.NewNetlinkRequest(unix.RTM_GETRULE, unix.NLM_F_DUMP|unix.NLM_F_REQUEST)
		req.AddData(nl.NewIfInfomsg(family))
		msgs, err := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWRULE)
		if err != nil {
			return err
		}
		for _, raw := range msgs {
			if len(raw) < unix.SizeofRtMsg {
				return errors.New("truncated rule message")
			}
			msg := nl.DeserializeRtMsg(raw)
			id := int(msg.Table)
			attrs, err := nl.ParseRouteAttr(raw[msg.Len():])
			if err != nil {
				return err
			}
			for _, attr := range attrs {
				if attr.Attr.Type == unix.RTA_TABLE {
					if len(attr.Value) != 4 {
						return errors.New("invalid rule table attribute")
					}
					id = int(nl.NativeEndian().Uint32(attr.Value))
				}
			}
			if id == table {
				count++
				if msg.Type != unix.RTN_UNICAST {
					return errors.New("non-lookup rule action is unsupported")
				}

				if msg.Flags != 0 || msg.Src_len != 0 || msg.Dst_len != 0 || msg.Tos != 0 {
					return errors.New("conditional rule header is unsupported")
				}
				for _, attr := range attrs {
					switch attr.Attr.Type {
					case nl.FRA_TABLE, nl.FRA_PRIORITY:
						if len(attr.Value) != 4 {
							return errors.New("invalid lookup attribute")
						}
					case nl.FRA_PROTOCOL:
						if len(attr.Value) != 1 {
							return errors.New("invalid rule protocol")
						}
					case nl.FRA_PAD:
						if len(attr.Value) != 0 {
							return errors.New("unexpected rule padding")
						}
					case nl.FRA_SUPPRESS_IFGROUP, nl.FRA_SUPPRESS_PREFIXLEN:
						if len(attr.Value) != 4 || nl.NativeEndian().Uint32(attr.Value) != ^uint32(0) {
							return errors.New("suppressed lookup is unsupported")
						}
					default:
						return fmt.Errorf("unsupported lookup attribute %d", attr.Attr.Type)
					}
				}
			}
		}
	}
	if count != expected {
		return errors.New("rule set changed during observation")
	}
	return nil
}

func canonicalRoute(r netlink.Route) netlink.Route {
	normalize := func(ip net.IP) net.IP {
		if v4 := ip.To4(); v4 != nil {
			return v4
		}
		return ip
	}
	r.Src = normalize(r.Src)
	r.Gw = normalize(r.Gw)
	if r.Dst != nil {
		dst := *r.Dst
		dst.IP = normalize(dst.IP)
		r.Dst = &dst
	}
	return r
}

func sameRoutes(a, b []netlink.Route) bool { return len(a) == len(b) && subsetRoutes(a, b) }

func subsetRoutes(a, b []netlink.Route) bool {
	used := make([]bool, len(b))
	for _, x := range a {
		found := false
		for i, y := range b {
			if !used[i] && reflect.DeepEqual(canonicalRoute(x), canonicalRoute(y)) {
				used[i] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func unchanged(p Plan) error {
	current, err := Observe(p.Config)
	if err != nil {
		return err
	}
	if !sameRoutes(p.Routes, current.Routes) || !reflect.DeepEqual(p.Rules, current.Rules) {
		return errors.New("source routing changed; retain guard and inspect original plan")
	}
	return nil
}

func shadow(p Plan) ([]netlink.Route, []netlink.Rule) {
	rs := append([]netlink.Route(nil), p.Routes...)
	qs := append([]netlink.Rule(nil), p.Rules...)
	for i := range rs {
		rs[i].Table = p.Config.ShadowTable
	}
	for i := range qs {
		qs[i].Table = p.Config.ShadowTable
		qs[i].Priority = p.Config.SourceTable + 1
	}
	return rs, qs
}

// Install requires an unused shadow table and priority in both families. It
// adds routes before rules and removes only its successful additions on error.
func Install(p Plan) (err error) {
	if err = unchanged(p); err != nil {
		return err
	}
	existing, err := routes(p.Config.ShadowTable)
	if err != nil {
		return err
	}
	if len(existing) != 0 {
		return errors.New("shadow routing table is occupied")
	}
	all, err := rules()
	if err != nil {
		return err
	}
	for _, r := range all {
		if r.Table == p.Config.ShadowTable || r.Priority == p.Config.SourceTable+1 || r.Goto == p.Config.SourceTable+1 {
			return errors.New("shadow rule table or priority is occupied or targeted")
		}
	}
	rs, qs := shadow(p)
	var addedRoutes []netlink.Route
	var addedRules []netlink.Rule
	defer func() {
		if err == nil {
			return
		}
		for i := len(addedRules) - 1; i >= 0; i-- {
			err = errors.Join(err, netlink.RuleDel(&addedRules[i]))
		}
		for i := len(addedRoutes) - 1; i >= 0; i-- {
			err = errors.Join(err, netlink.RouteDel(&addedRoutes[i]))
		}
	}()
	for i := range rs {
		if err = netlink.RouteAdd(&rs[i]); err != nil {
			return err
		}
		addedRoutes = append(addedRoutes, rs[i])
	}
	for i := range qs {
		if err = netlink.RuleAdd(&qs[i]); err != nil {
			return err
		}
		addedRules = append(addedRules, qs[i])
	}
	return Verify(p)
}

// Remove refuses to withdraw the fallback until the original routes and lookup
// rules are restored unchanged. A failed cleanup must retain its original plan;
// it is not evidence that the upgrade succeeded.
func Remove(p Plan) error {
	if err := unchanged(p); err != nil {
		return err
	}
	wantRoutes, wantRules := shadow(p)
	gotRoutes, err := routes(p.Config.ShadowTable)
	if err != nil {
		return err
	}
	if !subsetRoutes(gotRoutes, wantRoutes) {
		return errors.New("shadow routes changed; refusing cleanup")
	}
	all, err := rules()
	if err != nil {
		return err
	}
	var gotRules []netlink.Rule
	for _, r := range all {
		if r.Table == p.Config.ShadowTable || r.Priority == p.Config.SourceTable+1 || r.Goto == p.Config.SourceTable+1 {
			gotRules = append(gotRules, r)
		}
	}
	if err := lookupActions(p.Config.ShadowTable, len(gotRules)); err != nil {
		return err
	}
	used := make([]bool, len(wantRules))
	for _, got := range gotRules {
		found := false
		for i, want := range wantRules {
			if !used[i] && reflect.DeepEqual(got, want) {
				used[i] = true
				found = true
				break
			}
		}
		if !found {
			return errors.New("shadow rules changed; refusing cleanup")
		}
	}
	for i := range gotRules {
		if err := netlink.RuleDel(&gotRules[i]); err != nil {
			return err
		}
	}
	for i := range gotRoutes {
		if err := netlink.RouteDel(&gotRoutes[i]); err != nil {
			return err
		}
	}
	return nil
}

// Verify proves both the unchanged primary routing and the complete fallback
// are present before the caller stops the predecessor process.
func Verify(p Plan) error {
	if err := unchanged(p); err != nil {
		return err
	}
	wantRoutes, wantRules := shadow(p)
	got, err := routes(p.Config.ShadowTable)
	if err != nil {
		return err
	}
	if !sameRoutes(wantRoutes, got) {
		return errors.New("fallback routes are incomplete or changed")
	}
	all, err := rules()
	if err != nil {
		return err
	}
	var selected []netlink.Rule
	for _, r := range all {
		if r.Table == p.Config.ShadowTable || r.Priority == p.Config.SourceTable+1 || r.Goto == p.Config.SourceTable+1 {
			selected = append(selected, r)
		}
	}
	if !reflect.DeepEqual(wantRules, selected) {
		return errors.New("fallback rules are incomplete or changed")
	}
	return lookupActions(p.Config.ShadowTable, len(selected))
}
