package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Synthetic histories and incarnation callbacks exercise follower ordering.
// No live authentication, native routes, packet continuity or recovery deadline
// is established here; complete live inventory admission remains the caller's job.
func TestFollowerJournalRequiresRelaySwitchProof(t *testing.T) {
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	root, err := wgtypes.GeneratePrivateKey()
	must(err)
	site, err := wgtypes.GeneratePrivateKey()
	must(err)
	plan := receiveRestoreTestPlan(t, root, site)
	plan.Desired.Peers[0].RouteHosts = []string{"10.90.0.2", "fd90::2"}
	plan.Base, err = json.Marshal(plan.Desired)
	must(err)
	raw, err := json.Marshal(plan)
	must(err)
	spec, err := tunnel.ReceiveTransitionSpec(plan, "cluster", tunnel.HashPeerList(raw))
	must(err)
	at := time.Now().Truncate(time.Second)
	initial, err := handover.NewTransition(spec, at, handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
	must(err)
	proofs := func(tr handover.Transition, when time.Time, only string) []handover.AuthenticatedReceipt {
		v, err := tr.View()
		must(err)
		hash, err := v.Round.Digest()
		must(err)
		var result []handover.AuthenticatedReceipt
		for _, member := range v.Round.Members {
			if only != "" && member.NodeUID != only {
				continue
			}
			result = append(result, handover.AuthenticatedReceipt{AuthenticatedNodeUID: member.NodeUID, Receipt: handover.Receipt{RoundHash: hash, NodeUID: member.NodeUID, DeviceID: member.DeviceID, FromKey: member.FromKey, ToKey: member.ToKey, FromPeerKey: member.FromPeerKey, ToPeerKey: member.ToPeerKey, ObservedAt: when}})
		}
		return result
	}
	switching, err := initial.Advance(at.Add(time.Second), proofs(initial, at.Add(time.Second), ""))
	must(err)
	observed, err := switching.Observe(at.Add(2*time.Second), proofs(switching, at.Add(2*time.Second), "site-uid"))
	must(err)
	rollback, err := observed.Rollback(at.Add(3 * time.Second))
	must(err)
	rollbackObserved, err := rollback.Observe(at.Add(4*time.Second), proofs(rollback, at.Add(4*time.Second), "site-uid"))
	must(err)
	aborted, err := initial.Rollback(at.Add(time.Second))
	must(err)
	data := map[string][]byte{
		tunnel.PeerPublicKeyPrefix + plan.MachineName: []byte(plan.RootPublicKey),
		tunnel.NodePublicKeyPrefix + "site":           []byte(site.PublicKey().String()),
		tunnel.NodeAddressesPrefix + "site":           []byte("10.90.0.2,fd90::2"),
		tunnel.SiteAddressesPrefix + "follower":       []byte("10.90.0.4,fd90::4"),
	}
	for _, tc := range []struct {
		name              string
		tr                handover.Transition
		now               time.Time
		pass, incarnation bool
	}{
		{"prepare", initial, at.Add(time.Second), true, false},
		{"abort", aborted, at.Add(2 * time.Second), true, false},
		{"switch-without-proof", switching, at.Add(2 * time.Second), false, false},
		{"switch", observed, at.Add(3 * time.Second), true, true},
		{"rollback-without-proof", rollback, at.Add(4 * time.Second), false, false},
		{"rollback", rollbackObserved, at.Add(5 * time.Second), true, true},
		{"stale", observed, at.Add(30 * time.Second), false, false},
		{"expired", observed, at.Add(2 * time.Minute), false, false},
		{"future", initial, at.Add(-time.Second), false, false},
		{"restarted-relay", observed, at.Add(3 * time.Second), false, true},
		{"missing-verifier", observed, at.Add(3 * time.Second), false, false},
		{"local-receiver", observed, at.Add(3 * time.Second), false, false},
		{"local-cloud", observed, at.Add(3 * time.Second), false, false},
		{"withdrawn-root", observed, at.Add(3 * time.Second), false, false},
		{"replaced-root", observed, at.Add(3 * time.Second), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			history, err := tc.tr.Bytes()
			must(err)
			cm := corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveTransitionName(plan.MachineUID), Namespace: plan.Namespace, UID: "journal-uid", ResourceVersion: "1"}, Data: map[string]string{"plan.json": string(raw), "transition.json": string(history)}}
			checkpoint, err := json.Marshal(map[string]any{"version": 1, "object": cm})
			must(err)
			snapshot, err := (handover.Store{Name: cm.Name, ClusterUID: "cluster"}).RestoreCheckpoint(checkpoint)
			must(err)
			calls := 0
			restarted := errors.New("relay incarnation changed")
			verify := func(owner tunnel.ReceiveTransitionSite, receipt handover.Receipt) error {
				calls++
				if owner.NodeUID != "site-uid" || receipt.NodeUID != owner.NodeUID {
					t.Fatal("verified the wrong relay")
				}
				if tc.name == "restarted-relay" {
					return restarted
				}
				return nil
			}
			if tc.name == "missing-verifier" {
				verify = nil
			}
			uid := "follower-uid"
			if tc.name == "local-receiver" {
				uid = "site-uid"
			} else if tc.name == "local-cloud" {
				uid = "cloud-uid"
			}
			mesh := map[string][]byte{}
			for key, value := range data {
				mesh[key] = value
			}
			if tc.name == "withdrawn-root" {
				delete(mesh, tunnel.PeerPublicKeyPrefix+plan.MachineName)
			} else if tc.name == "replaced-root" {
				mesh[tunnel.PeerPublicKeyPrefix+plan.MachineName] = []byte(site.PublicKey().String())
			}
			path, err := admitReceiveFollowerJournalPath(mesh, "follower", uid, snapshot, "cluster", tc.now, verify)
			if (err == nil) != tc.pass || (calls > 0) != tc.incarnation {
				t.Fatalf("admission: path=%+v err=%v verifier calls=%d", path, err, calls)
			}
			if tc.name == "restarted-relay" && !errors.Is(err, restarted) {
				t.Fatal("lost incarnation failure")
			}
			if tc.pass && (path.Tunnel != nil || path.Transit == nil || path.Transit.Via != "10.90.0.2" || path.Transit.ViaOtherFamily != "fd90::2") {
				t.Fatal("admitted path lost its exact relay")
			}
		})
	}
}
