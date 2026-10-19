package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Synthetic records test exclusion planning only. Authentication, native
// ownership and safe address reuse require the independent admission paths.
func TestReleasedReceiveRouteExclusions(t *testing.T) {
	retired, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	active, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"none", "scope", "machine", "key", "active-root", "active-uid", "missing-finalization", "conflicting-exclusion"} {
		t.Run(fault, func(t *testing.T) {
			w := attachment.RemoteWithdrawalIntent{MeshNamespace: "test", MeshName: "mesh", MeshUID: "mesh-uid", MachineName: "retired", MachineUID: "retired", PublicKey: retired.PublicKey().String(), SourceVersion: "1", DrainIntent: "test/delete"}
			if fault == "scope" {
				w.MeshUID = "other"
			}
			if fault == "machine" {
				w.MachineUID = "other"
			}
			if fault == "key" {
				w.PublicKey = "invalid"
			}
			record, err := json.Marshal(map[string]any{"version": 1, "journalSHA256": tunnel.HashPeerList([]byte("history")), "withdrawal": w})
			if err != nil {
				t.Fatal(err)
			}
			p := tunnel.ReceiveJournalPublication{JournalUID: "retired-journal", Retirement: record, RetirementCompletion: json.RawMessage(`{}`), RetirementFinalization: json.RawMessage(`{}`)}
			if fault == "missing-finalization" {
				p.RetirementFinalization = nil
			}
			inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: "cluster", Namespace: "test", MeshName: "mesh", MeshUID: "mesh-uid", MachineUIDs: []string{"survivor"}, Publications: map[string]tunnel.ReceiveJournalPublication{"survivor": {JournalUID: "active-journal"}}, Released: map[string]tunnel.ReceiveJournalPublication{"retired": p}}
			plan := tunnel.ReceiveTransitionPlan{MachineUID: "survivor", RootPublicKey: active.PublicKey().String()}
			if fault == "active-root" {
				plan.RootPublicKey = retired.PublicKey().String()
			}
			routes := tunnel.ReceiveJournalRoutes{Plans: map[string]tunnel.ReceiveTransitionPlan{"survivor": plan}, Withdrawn: map[string]string{"pending": "pending-root"}}
			if fault == "active-uid" {
				routes.Plans["retired"] = plan
			}
			if fault == "conflicting-exclusion" {
				routes.Withdrawn["retired"] = "changed-root"
			}
			before, err := json.Marshal(routes)
			if err != nil {
				t.Fatal(err)
			}
			err = appendReleasedReceiveExclusions(inventory, &routes)
			if fault == "none" {
				if err != nil {
					t.Fatal(err)
				}
				if routes.Withdrawn["retired"] != retired.PublicKey().String() || routes.Withdrawn["pending"] != "pending-root" || !reflect.DeepEqual(routes.Plans["survivor"], plan) {
					t.Fatal("lost exclusion or changed active plan")
				}
			} else {
				if err == nil {
					t.Fatal("invalid released exclusion accepted")
				}
				after, e := json.Marshal(routes)
				if e != nil {
					t.Fatal(e)
				}
				if string(before) != string(after) {
					t.Fatal("rejection partially changed route intent")
				}
			}
		})
	}
}
