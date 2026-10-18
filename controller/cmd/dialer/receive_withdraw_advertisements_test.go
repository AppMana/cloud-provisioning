package main

import (
	"context"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"testing"
)

// Actual in-process GoBGP RIB, with no listening socket or kernel changes.
// Synthetic committed plan: proves IPv4/IPv6 targeted withdrawal, survivor MED
// preservation and retry. Does not prove external neighbor convergence, native
// forwarding cleanup, live source admission or crash-safe cache publication.
func TestReceiveAdvertisementWithdrawal(t *testing.T) {
	for _, fault := range []string{"none", "missing-owner", "missing-node", "cancelled", "untracked-path"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			speaker, err := startTransitSpeaker(ctx, -1, 64512, "10.90.0.1,fd90::1")
			if err != nil {
				t.Fatal(err)
			}
			defer speaker.stop(context.Background())
			site, _ := wgtypes.GeneratePrivateKey()
			root, _ := wgtypes.GeneratePrivateKey()
			plan := receiveRestoreTestPlan(t, root, site)
			routes := []transitRoute{{prefix: "172.29.0.1/32", med: 50}, {prefix: "fd29::1/128", med: 50}, {prefix: "172.29.0.3/32", med: 90}, {prefix: "fd29::3/128", med: 90}}
			if err := speaker.reconcile(ctx, nil, routes); err != nil {
				t.Fatal(err)
			}
			target, node := plan.MachineUID, "site-uid"
			switch fault {
			case "missing-owner":
				target = "missing"
			case "missing-node":
				node = "missing"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "untracked-path":
				delete(speaker.advertised, "172.29.0.1/32")
			}
			err = withdrawReceiveAdvertisements(ctx, speaker, []tunnel.ReceiveTransitionPlan{plan}, target, node)
			if fault != "none" {
				if err == nil {
					t.Fatal("invalid or incomplete withdrawal succeeded")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := withdrawReceiveAdvertisements(ctx, speaker, []tunnel.ReceiveTransitionPlan{plan}, target, node); err != nil {
					t.Fatal("retry", err)
				}
			}
			for i, route := range routes {
				med, err := advertisedMED(context.Background(), speaker, route.prefix)
				absent := i < 2 && (fault == "none" || (fault == "untracked-path" && i == 1))
				if absent {
					if err == nil {
						t.Fatal("retired prefix still present")
					}
				} else if err != nil || med != route.med {
					t.Fatal("preserved path changed", route.prefix, err)
				}
			}
		})
	}
}
