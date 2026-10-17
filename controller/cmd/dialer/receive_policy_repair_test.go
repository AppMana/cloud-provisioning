package main

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Called only by the isolated native fixture, before its protected-stream run.
func verifyNativePolicyRecovery(t *testing.T, devices receiverDevices, cfg config, key, peer wgtypes.Key, route netlink.Route) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	selected := cfg
	selected.receiveRouteScope = map[string]bool{route.Dst.String(): true}
	groups := []receiveAdvertisementGroup{{config: selected, own: []string{"10.77.0.1"}, legacy: true, destinations: []net.IPNet{*route.Dst}, peer: tunnel.PeerSpec{PublicKey: peer.String(), WGAllowedIPs: []string{route.Dst.String(), "10.77.2.0/24"}}}}
	remove := func() {
		for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
			rules, err := netlink.RuleList(family)
			must(err)
			for i := range rules {
				if matchesTunnelRule(rules[i], family, cfg.routeTable, cfg.routeTable, 0) || matchesTunnelRule(rules[i], family, unix.RT_TABLE_MAIN, exemptRulePriority, uint32(cfg.fwmark)) {
					must(netlink.RuleDel(&rules[i]))
				}
			}
		}
	}
	remove()
	for _, fault := range []string{"identity", "default", "unpublished", "foreign-protocol", "foreign-rule", "revalidation", "cancelled"} {
		t.Run("policy-recovery-"+fault, func(t *testing.T) {
			ctx := context.Background()
			root := key
			check := func() error { return nil }
			switch fault {
			case "identity":
				root, _ = wgtypes.GeneratePrivateKey()
			case "default", "unpublished", "foreign-protocol":
				foreign := route
				_, foreign.Dst, _ = net.ParseCIDR("10.88.0.0/24")
				if fault == "default" {
					foreign.Dst = nil
				}
				if fault == "foreign-protocol" {
					foreign.Dst = route.Dst
					foreign.Priority = 42
					foreign.Protocol = unix.RTPROT_STATIC
				}
				must(netlink.RouteAdd(&foreign))
				defer netlink.RouteDel(&foreign)
			case "foreign-rule":
				foreign := netlink.NewRule()
				foreign.Family, foreign.Table, foreign.Priority = netlink.FAMILY_V6, cfg.routeTable, cfg.routeTable
				foreign.Mark = 123
				must(netlink.RuleAdd(foreign))
				defer netlink.RuleDel(foreign)
			case "revalidation":
				check = func() error { return fmt.Errorf("publication changed") }
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := restoreReceivePolicyRules(ctx, devices, cfg, root, groups, check); err == nil {
				t.Fatal("unsafe policy recovery accepted")
			}
			// A second-family collision must not partially activate IPv4.
			rules, err := netlink.RuleList(netlink.FAMILY_V4)
			must(err)
			missing, err := planTunnelRules(rules, netlink.FAMILY_V4, cfg.routeTable, cfg.fwmark)
			must(err)
			if len(missing) != 2 {
				t.Fatal("failed recovery changed policy rules")
			}
		})
	}
	checks := 0
	check := func() error { checks++; return nil }
	must(restoreReceivePolicyRules(context.Background(), devices, cfg, key, groups, check))
	if checks != 2 {
		t.Fatalf("expected admission and completion revalidation, got %d", checks)
	}
	must(verifyReceiveRouteInstallation(selected, []net.IPNet{*route.Dst}, nil, []string{"10.77.0.1"}))
	must(restoreReceivePolicyRules(context.Background(), devices, cfg, key, groups, check))
	if checks != 2 {
		t.Fatal("intact policy should not need recovery")
	}
}
