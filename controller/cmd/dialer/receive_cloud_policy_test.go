package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestCloudReceivePolicyProjection(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, fault := range []string{"valid", "desired-publication", "withdrawn-remote", "identity", "publication", "replacement", "unfinished", "receiver", "generation", "trailing"} {
			t.Run(fmt.Sprintf("legacy-%t/%s", legacy, fault), func(t *testing.T) {
				root, err := wgtypes.GeneratePrivateKey()
				if err != nil {
					t.Fatal(err)
				}
				site, err := wgtypes.GeneratePrivateKey()
				if err != nil {
					t.Fatal(err)
				}
				plan := receiveRestoreTestPlan(t, root, site)
				var retiredPublic string
				if fault == "withdrawn-remote" {
					retired, err := wgtypes.GeneratePrivateKey()
					if err != nil {
						t.Fatal(err)
					}
					retiredPublic = retired.PublicKey().String()
					peer := tunnel.PeerSpec{Remote: true, PublicKey: retiredPublic, WGAllowedIPs: []string{"172.31.0.1/32"}}
					plan.Desired.Peers = append(plan.Desired.Peers, peer)
					var base tunnel.PeerListDoc
					if err := json.Unmarshal(plan.Base, &base); err != nil {
						t.Fatal(err)
					}
					base.Peers = append(base.Peers, peer)
					plan.Base, err = json.Marshal(base)
					if err != nil {
						t.Fatal(err)
					}
				}
				plan.Desired.APIServers = []string{"https://192.0.2.10:6443"}
				raw, err := json.Marshal(plan)
				if err != nil {
					t.Fatal(err)
				}
				spec, err := tunnel.ReceiveTransitionSpec(plan, "cluster", fmt.Sprintf("%x", sha256.Sum256(raw)))
				if err != nil {
					t.Fatal(err)
				}
				spec.Phase = handover.Retire
				view := handover.View{Round: handover.Round{Spec: spec}, Complete: true, ServingGeneration: spec.ToGeneration}
				intent := tunnel.ReceiveJournalRoutes{Plans: map[string]tunnel.ReceiveTransitionPlan{plan.MachineUID: plan}, Views: map[string]handover.View{}, Legacy: map[string]tunnel.ReceiveTransitionPlan{}}
				if retiredPublic != "" {
					intent.Withdrawn = map[string]tunnel.ReceiveTransitionPlan{"retired": {MachineUID: "retired", RootPublicKey: retiredPublic}}
				}
				intent.Receivers, err = tunnel.PlanReceiveRoutes(plan)
				if err != nil {
					t.Fatal(err)
				}
				if legacy {
					view.ServingGeneration = spec.FromGeneration
					intent.Legacy[plan.MachineUID] = plan
					intent.Receivers[plan.CloudNodeUID] = map[string]tunnel.ReceiveRouteSelection{}
				}
				adoption := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: plan.Namespace, Name: plan.AdoptionName, UID: types.UID(plan.AdoptionUID), ResourceVersion: "1"}, Data: map[string][]byte{tunnel.CloudPeersKey: plan.Base}}
				cfg := config{nodeName: plan.CloudNodeName}
				switch fault {
				case "desired-publication":
					adoption.Data[tunnel.CloudPeersKey], err = json.Marshal(plan.Desired)
					if err != nil {
						t.Fatal(err)
					}
				case "identity":
					cfg.nodeName = "replacement"
				case "publication":
					adoption.Data[tunnel.CloudPeersKey] = []byte(`{"peers":[]}`)
				case "replacement":
					adoption.UID = "replacement"
				case "unfinished":
					view.Complete = false
				case "receiver":
					intent.Receivers[plan.CloudNodeUID]["192.0.2.1/32"] = tunnel.ReceiveRouteSelection{Interface: "foreign"}
				case "generation":
					view.ServingGeneration = "receivers:other"
				case "trailing":
					adoption.Data[tunnel.CloudPeersKey] = append(append([]byte{}, plan.Base...), []byte(` {}`)...)
				}
				intent.Views[plan.MachineUID] = view
				groups, err := planCloudReceivePolicy(cfg, root, plan, view, plan.CloudNodeUID, intent, adoption)
				if fault != "valid" && fault != "desired-publication" && fault != "withdrawn-remote" {
					if err == nil {
						t.Fatal("unsafe projection accepted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(groups) != 1 || len(groups[0].destinations) != 2 || groups[0].legacy != legacy {
					t.Fatalf("wrong projection: %+v", groups)
				}
				if !legacy && len(groups[0].config.receiveRoutes) != 2 {
					t.Fatal("selected receiver routes omitted")
				}
			})
		}
	}
}

// Called only inside the guarded, isolated native restoration fixture, before
// its second endpoint adds routes to the shared test namespace.
func verifyNativeCloudPolicyRecovery(t *testing.T, devices receiverDevices, cfg config, root wgtypes.Key, plan tunnel.ReceiveTransitionPlan, view handover.View, intent tunnel.ReceiveJournalRoutes) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	adoption := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: plan.Namespace, Name: plan.AdoptionName, UID: types.UID(plan.AdoptionUID), ResourceVersion: "1"}, Data: map[string][]byte{tunnel.CloudPeersKey: plan.Base}}
	groups, err := planCloudReceivePolicy(cfg, root, plan, view, plan.CloudNodeUID, intent, adoption)
	must(err)
	checks := 0
	must(restoreReceivePolicyRules(context.Background(), devices, cfg, root, groups, func() error { checks++; return nil }))
	if checks != 2 {
		t.Fatal("cloud policy recovery omitted revalidation")
	}
	must(verifyReceivePolicyRules(cfg))
	// Restore the fixture's missing-policy starting condition. Maintenance of
	// selected paths below must still not install shared rules itself.
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
