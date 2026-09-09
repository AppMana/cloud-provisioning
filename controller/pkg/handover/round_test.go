package handover

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

var epoch = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T) Round {
	t.Helper()
	key := func() string {
		p, e := wgtypes.GeneratePrivateKey()
		if e != nil {
			t.Fatal(e)
		}
		return p.PublicKey().String()
	}
	r, e := NewRound(Spec{ClusterUID: "cluster-uid", TransitionUID: "transition-uid", PlanHash: strings.Repeat("a", 64), FromGeneration: "A", ToGeneration: "B", Phase: Prepare, Members: []Member{{"node-2", key(), key()}, {"node-1", key(), key()}}}, epoch, time.Minute, 10*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func receipts(t *testing.T, r Round, at time.Time) []AuthenticatedReceipt {
	t.Helper()
	hash, e := r.Digest()
	if e != nil {
		t.Fatal(e)
	}
	out := []AuthenticatedReceipt{}
	for _, m := range r.Members {
		out = append(out, AuthenticatedReceipt{m.NodeUID, Receipt{hash, m.NodeUID, m.FromKey, m.ToKey, at}})
	}
	return out
}
func TestCurrentCompleteRoundAndRestart(t *testing.T) {
	r := fixture(t)
	proof := receipts(t, r, epoch.Add(time.Second))
	if e := r.Ready(epoch.Add(2*time.Second), proof); e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	restarted, e := DecodeRound(raw)
	if e != nil {
		t.Fatal(e)
	}
	if e = restarted.Ready(epoch.Add(3*time.Second), proof); e != nil {
		t.Fatal("persisted round lost receipt binding", e)
	}
	// Membership enumeration and local timezone are not transition changes.
	reversed := restarted
	reversed.Members = append([]Member(nil), restarted.Members...)
	reversed.Members[0], reversed.Members[1] = reversed.Members[1], reversed.Members[0]
	reversed.IssuedAt = reversed.IssuedAt.In(time.FixedZone("offset", 3600))
	reversed.ExpiresAt = reversed.ExpiresAt.In(time.FixedZone("offset", 3600))
	a, _ := r.Digest()
	b, e := reversed.Digest()
	if e != nil || a != b {
		t.Fatal("canonical round changed", e)
	}
	if e = reversed.Ready(epoch.Add(3*time.Second), proof); e != nil {
		t.Fatal(e)
	}
	if reversed.Members[0].NodeUID == restarted.Members[0].NodeUID {
		t.Fatal("validation mutated caller membership")
	}
}
func TestReceiptsCannotAdvanceChangedIntent(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Round)
	}{
		{"cluster", func(r *Round) { r.ClusterUID = "other-cluster" }},
		{"transition", func(r *Round) { r.TransitionUID = "other-transition" }},
		{"graph", func(r *Round) { r.PlanHash = strings.Repeat("b", 64) }},
		{"phase", func(r *Round) { r.Phase = Switch }},
		{"generation", func(r *Round) { r.ToGeneration = "C" }},
		{"replacement UID", func(r *Round) { r.Members[0].NodeUID = "replacement-uid" }},
		{"fresh attempt", func(r *Round) {
			next, e := NewRound(r.Spec, epoch, time.Minute, 10*time.Second)
			if e != nil {
				panic(e)
			}
			*r = next
		}},
		{"rollback", func(r *Round) {
			r.FromGeneration, r.ToGeneration = r.ToGeneration, r.FromGeneration
			for i := range r.Members {
				r.Members[i].FromKey, r.Members[i].ToKey = r.Members[i].ToKey, r.Members[i].FromKey
			}
			r.Phase = Switch
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := fixture(t)
			proof := receipts(t, r, epoch.Add(time.Second))
			test.change(&r)
			if e := r.Ready(epoch.Add(2*time.Second), proof); e == nil {
				t.Fatal("old receipts accepted after intent changed")
			}
		})
	}
}
func TestReceiptIdentityCompletenessAndFreshness(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func([]AuthenticatedReceipt) []AuthenticatedReceipt
	}{
		{"missing", func(p []AuthenticatedReceipt) []AuthenticatedReceipt { return p[:1] }},
		{"duplicate", func(p []AuthenticatedReceipt) []AuthenticatedReceipt { p[1] = p[0]; return p }},
		{"unknown", func(p []AuthenticatedReceipt) []AuthenticatedReceipt {
			p[0].Receipt.NodeUID = "unknown"
			p[0].AuthenticatedNodeUID = "unknown"
			return p
		}},
		{"forged identity", func(p []AuthenticatedReceipt) []AuthenticatedReceipt {
			p[0].AuthenticatedNodeUID = p[1].AuthenticatedNodeUID
			return p
		}},
		{"wrong old device", func(p []AuthenticatedReceipt) []AuthenticatedReceipt {
			p[0].Receipt.FromKey = p[1].Receipt.FromKey
			return p
		}},
		{"wrong new device", func(p []AuthenticatedReceipt) []AuthenticatedReceipt {
			p[0].Receipt.ToKey = p[1].Receipt.ToKey
			return p
		}},
		{"wrong round", func(p []AuthenticatedReceipt) []AuthenticatedReceipt {
			p[0].Receipt.RoundHash = strings.Repeat("0", 64)
			return p
		}},
		{"before publication", func(p []AuthenticatedReceipt) []AuthenticatedReceipt {
			p[0].Receipt.ObservedAt = epoch.Add(-time.Second)
			return p
		}},
		{"future", func(p []AuthenticatedReceipt) []AuthenticatedReceipt {
			p[0].Receipt.ObservedAt = epoch.Add(21 * time.Second)
			return p
		}},
		{"stale", func(p []AuthenticatedReceipt) []AuthenticatedReceipt {
			p[0].Receipt.ObservedAt = epoch.Add(time.Second)
			return p
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := fixture(t)
			proof := test.change(receipts(t, r, epoch.Add(15*time.Second)))
			if e := r.Ready(epoch.Add(20*time.Second), proof); e == nil {
				t.Fatal("invalid receipt set accepted")
			}
		})
	}
	r := fixture(t)
	proof := receipts(t, r, epoch.Add(time.Second))
	if e := r.Ready(epoch.Add(11*time.Second), proof); e != nil {
		t.Fatal("inclusive freshness boundary rejected", e)
	}
	for _, now := range []time.Time{epoch.Add(-time.Second), r.ExpiresAt} {
		if e := r.Ready(now, proof); e == nil {
			t.Fatal("non-current round accepted")
		}
	}
}
func TestRejectInvalidRoundAndDeviceReuse(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Round)
	}{
		{"empty cluster", func(r *Round) { r.ClusterUID = "" }},
		{"whitespace UID", func(r *Round) { r.TransitionUID = " x" }},
		{"same generation", func(r *Round) { r.ToGeneration = r.FromGeneration }},
		{"malformed graph", func(r *Round) { r.PlanHash = "abc" }},
		{"uppercase graph", func(r *Round) { r.PlanHash = strings.Repeat("A", 64) }},
		{"malformed nonce", func(r *Round) { r.Nonce = "not-a-nonce" }},
		{"unknown phase", func(r *Round) { r.Phase = "promote" }},
		{"zero publication", func(r *Round) { r.IssuedAt = time.Time{} }},
		{"expired at publication", func(r *Round) { r.ExpiresAt = r.IssuedAt }},
		{"zero age", func(r *Round) { r.MaxReceiptAge = 0 }},
		{"age exceeds lifetime", func(r *Round) { r.MaxReceiptAge = 2 * time.Minute }},
		{"empty members", func(r *Round) { r.Members = nil }},
		{"duplicate UID", func(r *Round) { r.Members[1].NodeUID = r.Members[0].NodeUID }},
		{"empty UID", func(r *Round) { r.Members[0].NodeUID = "" }},
		{"bad key", func(r *Round) { r.Members[0].FromKey = "not-a-key" }},
		{"zero key", func(r *Round) { r.Members[0].FromKey = (wgtypes.Key{}).String() }},
		{"same node reuse", func(r *Round) { r.Members[0].ToKey = r.Members[0].FromKey }},
		{"cross-node reuse", func(r *Round) { r.Members[1].ToKey = r.Members[0].FromKey }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := fixture(t)
			test.change(&r)
			if _, e := r.Digest(); e == nil {
				t.Fatal("invalid round hashed")
			}
			if e := r.Ready(epoch, nil); e == nil {
				t.Fatal("invalid round accepted")
			}
		})
	}
}
func TestAllPhasesAndConstructorCopiesMembership(t *testing.T) {
	for _, phase := range []Phase{Prepare, Switch, Drain, Retire} {
		r := fixture(t)
		spec := r.Spec
		spec.Phase = phase
		round, e := NewRound(spec, epoch, time.Minute, time.Second)
		if e != nil {
			t.Fatal(e)
		}
		spec.Members[0].NodeUID = "changed-caller-state"
		if round.Members[0].NodeUID == "changed-caller-state" {
			t.Fatal("round aliases caller membership")
		}
		if e = round.Ready(epoch, receipts(t, round, epoch)); e != nil {
			t.Fatal(e)
		}
	}
	spec := fixture(t).Spec
	if _, e := NewRound(spec, epoch, -time.Second, time.Second); e == nil {
		t.Fatal("negative round lifetime accepted")
	}
}

func TestPersistedRoundRejectsUnknownOrTrailingState(t *testing.T) {
	r := fixture(t)
	raw, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	for _, bad := range [][]byte{[]byte(`{"newProtocolField":true}`), append(append([]byte(nil), raw...), []byte(` {}`)...), []byte(`null`), []byte(`{`)} {
		if _, e := DecodeRound(bad); e == nil {
			t.Fatal("invalid persisted state accepted")
		}
	}
}

func TestReceiptPayloadCannotSupplyAuthenticatedIdentity(t *testing.T) {
	r := fixture(t)
	proof := receipts(t, r, epoch)
	raw, e := json.Marshal(map[string]any{"AuthenticatedNodeUID": proof[0].AuthenticatedNodeUID, "Receipt": proof[0].Receipt})
	if e != nil {
		t.Fatal(e)
	}
	var decoded AuthenticatedReceipt
	if e = json.Unmarshal(raw, &decoded); e != nil {
		t.Fatal(e)
	}
	if decoded.AuthenticatedNodeUID != "" {
		t.Fatal("wire payload supplied authenticated identity")
	}
	proof[0] = decoded
	if e = r.Ready(epoch, proof); e == nil {
		t.Fatal("unauthenticated payload accepted")
	}
}

func TestSwitchReceiptCannotAuthorizeReverseDirection(t *testing.T) {
	r := fixture(t)
	r.Phase = Switch
	proof := receipts(t, r, epoch)
	r.FromGeneration, r.ToGeneration = r.ToGeneration, r.FromGeneration
	for i := range r.Members {
		r.Members[i].FromKey, r.Members[i].ToKey = r.Members[i].ToKey, r.Members[i].FromKey
	}
	// Even before a fresh nonce is minted, a select-B receipt cannot select A.
	if e := r.Ready(epoch, proof); e == nil {
		t.Fatal("forward switch receipts authorized rollback")
	}
}
func TestMembershipChangeInvalidatesSurvivorReceipt(t *testing.T) {
	r := fixture(t)
	previous := receipts(t, r, epoch)
	r.Members[1].NodeUID = "replacement-node-uid"
	current := receipts(t, r, epoch)
	current[0] = previous[0] // unchanged node and keys, but an older membership view
	if e := r.Ready(epoch, current); e == nil {
		t.Fatal("survivor did not acknowledge changed membership")
	}
}
