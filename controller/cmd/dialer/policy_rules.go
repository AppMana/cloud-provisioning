package main

import (
	"fmt"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func matchesTunnelRule(r netlink.Rule, family, table, priority int, mark uint32) bool {
	maskOK := r.Mask == nil || (mark != 0 && *r.Mask == ^uint32(0)) || (mark == 0 && *r.Mask == 0)
	return r.Family == family && r.Table == table && r.Priority == priority && r.Mark == mark && maskOK && r.Src == nil && r.Dst == nil && !r.Invert && r.IifName == "" && r.OifName == "" && r.Tos == 0 && r.TunID == 0 && r.Goto < 0 && r.Flow < 0 && r.SuppressIfgroup < 0 && r.SuppressPrefixlen < 0 && r.Dport == nil && r.Sport == nil && r.IPProto == 0 && r.UIDRange == nil && (r.Type == 0 || r.Type == unix.RTN_UNICAST)
}

// Plan only the missing rules. Existing rules are never rewritten or removed
// to make a collision disappear. Unrelated policy priorities remain untouched.
func planTunnelRules(rules []netlink.Rule, family, table, fwmark int) ([]netlink.Rule, error) {
	if table <= exemptRulePriority || table == unix.RT_TABLE_MAIN || table == unix.RT_TABLE_LOCAL || table == unix.RT_TABLE_DEFAULT || fwmark < 0 {
		return nil, fmt.Errorf("invalid tunnel policy table or mark")
	}
	foundTable, foundMark := false, fwmark == 0
	for _, r := range rules {
		if r.Family != family {
			continue
		}
		if r.Table == table || r.Priority == table {
			if !matchesTunnelRule(r, family, table, table, 0) || foundTable {
				return nil, fmt.Errorf("foreign or duplicate rule collides with tunnel table %d", table)
			}
			foundTable = true
		}
		if fwmark > 0 && r.Priority == exemptRulePriority {
			if matchesTunnelRule(r, family, unix.RT_TABLE_MAIN, exemptRulePriority, uint32(fwmark)) {
				if foundMark {
					return nil, fmt.Errorf("duplicate tunnel mark exemption")
				}
				foundMark = true
			} else if r.Mark != 0 && r.Mark != uint32(fwmark) && matchesTunnelRule(r, family, unix.RT_TABLE_MAIN, exemptRulePriority, r.Mark) {
				continue // An exact disjoint mark exemption can belong to another tunnel.
			} else {
				return nil, fmt.Errorf("foreign rule collides with tunnel mark exemption")
			}
		}
	}
	var missing []netlink.Rule
	if !foundMark {
		r := netlink.NewRule()
		r.Family = family
		r.Table = unix.RT_TABLE_MAIN
		r.Priority = exemptRulePriority
		r.Mark = uint32(fwmark)
		missing = append(missing, *r)
	}
	if !foundTable {
		r := netlink.NewRule()
		r.Family = family
		r.Table = table
		r.Priority = table
		missing = append(missing, *r)
	}
	return missing, nil
}

// netlink v1.3.1 RuleList does not populate Rule.Type from the kernel action.
// Check that omitted field directly; a blackhole with the same mark and table
// must never be mistaken for the exemption that keeps encrypted packets alive.
func validateTunnelRuleActions(family, table, fwmark int) error {
	return validatePolicyRuleActions(family, table, fwmark, false)
}

func validatePolicyRuleActions(family, table, fwmark int, source bool) error {
	req := nl.NewNetlinkRequest(unix.RTM_GETRULE, unix.NLM_F_DUMP|unix.NLM_F_REQUEST)
	req.AddData(nl.NewIfInfomsg(family))
	messages, err := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWRULE)
	if err != nil {
		return err
	}
	for _, raw := range messages {
		if len(raw) < unix.SizeofRtMsg {
			return fmt.Errorf("short policy rule response")
		}
		msg := nl.DeserializeRtMsg(raw)
		attrs, err := nl.ParseRouteAttr(raw[msg.Len():])
		if err != nil {
			return err
		}
		priority := 0
		for _, attr := range attrs {
			if attr.Attr.Type == unix.FRA_PRIORITY {
				if len(attr.Value) != 4 {
					return fmt.Errorf("invalid policy priority response")
				}
				priority = int(nl.NativeEndian().Uint32(attr.Value))
			}
		}
		if priority != table && !(fwmark > 0 && priority == exemptRulePriority) {
			continue
		}
		if msg.Type != unix.FR_ACT_TO_TBL {
			return fmt.Errorf("non-lookup policy action collides with tunnel priority %d", priority)
		}
		// Reject selectors outside the permitted lookup shape, including
		// attributes this version of the typed reader does not understand.
		for _, attr := range attrs {
			if source && attr.Attr.Type == unix.FRA_SRC {
				continue
			}
			switch attr.Attr.Type {
			case nl.FRA_PRIORITY, nl.FRA_TABLE, nl.FRA_FWMARK, nl.FRA_FWMASK, nl.FRA_PROTOCOL, nl.FRA_SUPPRESS_IFGROUP, nl.FRA_SUPPRESS_PREFIXLEN:
			default:
				return fmt.Errorf("qualified policy rule collides with tunnel priority %d", priority)
			}
		}
	}
	return nil
}
