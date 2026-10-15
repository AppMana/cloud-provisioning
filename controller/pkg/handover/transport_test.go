package handover

import (
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func transportFixture(t *testing.T) Transition {
	t.Helper()
	key := func() string {
		p, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		return p.PublicKey().String()
	}
	root, first, second, siteA, siteB := key(), key(), key(), key(), key()
	spec := fixture(t).Spec
	spec.Members = []Member{
		{NodeUID: "cloud", DeviceID: "site-a", FromKey: root, ToKey: first, FromPeerKey: siteA, ToPeerKey: siteA},
		{NodeUID: "cloud", DeviceID: "site-b", FromKey: root, ToKey: second, FromPeerKey: siteB, ToPeerKey: siteB},
		{NodeUID: "site-a", DeviceID: "cloud", FromKey: siteA, ToKey: siteA, FromPeerKey: root, ToPeerKey: first},
		{NodeUID: "site-b", DeviceID: "cloud", FromKey: siteB, ToKey: siteB, FromPeerKey: root, ToPeerKey: second},
	}
	tr, err := NewTransition(spec, epoch, Policy{time.Minute, 10 * time.Second, 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestTransportMembersSurviveRestartAndRollback(t *testing.T) {
	tr := transportFixture(t)
	initial := view(t, tr).Round
	tr = step(t, tr, epoch.Add(time.Second))
	switched := view(t, tr)
	if switched.Round.Phase != Switch || len(switched.Round.Members) != 4 {
		t.Fatal("transport membership lost during switch")
	}
	rolled, err := tr.Rollback(epoch.Add(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := rolled.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := DecodeTransition(raw)
	if err != nil {
		t.Fatal(err)
	}
	rollback := view(t, restarted)
	if !rollback.RollbackRequested || rollback.Round.FromGeneration != initial.ToGeneration || rollback.Round.ToGeneration != initial.FromGeneration {
		t.Fatal("rollback lost generation direction")
	}
	for i, member := range rollback.Round.Members {
		before := initial.Members[i]
		if member.NodeUID != before.NodeUID || member.DeviceID != before.DeviceID || member.FromKey != before.ToKey || member.ToKey != before.FromKey || member.FromPeerKey != before.ToPeerKey || member.ToPeerKey != before.FromPeerKey {
			t.Fatal("rollback did not reverse the exact transport")
		}
	}
	if err := rollback.Round.Ready(epoch.Add(3*time.Second), receipts(t, switched.Round, epoch.Add(time.Second))); err == nil {
		t.Fatal("old switch receipts authorized rollback")
	}
	restarted = step(t, restarted, epoch.Add(3*time.Second))
	if view(t, restarted).Round.Phase != Drain {
		t.Fatal("rollback could not enter drain")
	}
	restarted = step(t, restarted, epoch.Add(8*time.Second))
	if view(t, restarted).Round.Phase != Retire {
		t.Fatal("rollback did not retain its drain interval")
	}
	restarted = step(t, restarted, epoch.Add(9*time.Second))
	completed := view(t, restarted)
	if !completed.Complete || completed.ServingGeneration != initial.FromGeneration {
		t.Fatal("rollback completed on the wrong generation")
	}

}

func TestTransportReceiptsCannotSubstituteAnotherPathOnTheSameNode(t *testing.T) {
	round := view(t, transportFixture(t)).Round
	for _, fault := range []string{"device", "peer", "duplicate", "authenticated-node"} {
		proof := receipts(t, round, epoch.Add(time.Second))
		switch fault {
		case "device":
			proof[0].Receipt.DeviceID = "unknown"
		case "peer":
			proof[0].Receipt.ToPeerKey = proof[1].Receipt.ToPeerKey
		case "duplicate":
			proof[0] = proof[1]
		case "authenticated-node":
			proof[0].AuthenticatedNodeUID = "another-node"
		}
		if err := round.Ready(epoch.Add(2*time.Second), proof); err == nil {
			t.Fatalf("accepted invalid transport proof: %s", fault)
		}
	}
}

func TestTransportMembershipRejectsAmbiguousDeviceOwnership(t *testing.T) {
	for _, fault := range []string{"duplicate-device", "unchanged-pair", "missing-peer", "mixed-model", "shared-key-across-nodes"} {
		round := view(t, transportFixture(t)).Round
		switch fault {
		case "duplicate-device":
			round.Members[1].DeviceID = round.Members[0].DeviceID
		case "unchanged-pair":
			round.Members[0].ToKey = round.Members[0].FromKey
		case "missing-peer":
			round.Members[0].ToPeerKey = ""
		case "mixed-model":
			round.Members[0].DeviceID = ""
		case "shared-key-across-nodes":
			round.Members[1].NodeUID = "different-cloud"
		}
		if _, err := round.Digest(); err == nil {
			t.Fatalf("accepted ambiguous transport: %s", fault)
		}
	}
}
