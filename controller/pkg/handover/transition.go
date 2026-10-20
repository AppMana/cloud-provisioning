package handover

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"time"
)

// Policy's drain interval is a deployment's explicit packet-lifetime bound,
// not an inferred guarantee. Fresh native drain receipts remain mandatory.
type Policy struct {
	RoundLifetime time.Duration `json:"roundLifetime"`
	MaxReceiptAge time.Duration `json:"maxReceiptAge"`
	DrainInterval time.Duration `json:"drainInterval"`
}

type operation string

const (
	advance  operation = "advance"
	observe  operation = "observe"
	rollback operation = "rollback"
	renew    operation = "renew"
)

type event struct {
	Operation operation `json:"operation"`
	At        time.Time `json:"at"`
	// These receipts were authenticated before the controller committed them.
	// Persisted journals must be read from controller-owned, trusted storage.
	Receipts []Receipt `json:"receipts,omitempty"`
	Next     *Round    `json:"next,omitempty"`
}
type journal struct {
	Version      int          `json:"version"`
	Predecessor  *Predecessor `json:"predecessor,omitempty"`
	PlanRequired bool         `json:"planRequired,omitempty"`
	Policy       Policy       `json:"policy"`
	Initial      Round        `json:"initial"`
	Events       []event      `json:"events"`
}

// Transition is immutable through its public API. Candidate operations must be
// committed before their rounds are published or any native action is taken.
type Transition struct{ j journal }
type View struct {
	Observed          []Receipt // authenticated observations for this exact current round
	Round             Round
	RollbackRequested bool
	Complete          bool
	ServingGeneration string // populated only after verified completion
	DrainUntil        time.Time
}

func NewTransition(spec Spec, now time.Time, policy Policy) (Transition, error) {
	if spec.Phase != Prepare {
		return Transition{}, fmt.Errorf("transition must begin with prepare")
	}
	r, e := NewRound(spec, now, policy.RoundLifetime, policy.MaxReceiptAge)
	if e != nil {
		return Transition{}, e
	}
	t := Transition{journal{Version: 1, Policy: policy, Initial: r, Events: []event{}}}
	_, e = t.View()
	return t, e
}

func DecodeTransition(raw []byte) (Transition, error) {
	var j journal
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&j); e != nil {
		return Transition{}, e
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return Transition{}, fmt.Errorf("trailing transition state")
	}
	t := Transition{j}
	_, e := t.View()
	return t, e
}
func (t Transition) Bytes() ([]byte, error) {
	if _, e := t.View(); e != nil {
		return nil, e
	}
	return json.Marshal(t.j)
}

// View replays and verifies history on every load. It never turns elapsed time
// or a persisted desired phase into evidence of a completed native operation.
func (t Transition) View() (View, error) {
	p := t.j.Policy
	if (t.j.Version != 1 && t.j.Version != 2) || p.RoundLifetime <= 0 || p.MaxReceiptAge <= 0 || p.MaxReceiptAge > p.RoundLifetime || p.DrainInterval <= 0 {
		return View{}, fmt.Errorf("invalid transition version or policy")
	}
	if (t.j.Version == 1 && t.j.Predecessor != nil) || (t.j.Version == 2 && (t.j.Predecessor == nil || !t.j.Predecessor.valid())) {
		return View{}, fmt.Errorf("invalid predecessor binding")
	}
	if t.j.Predecessor != nil && t.j.Initial.FromGeneration != t.j.Predecessor.ServingGeneration {
		return View{}, fmt.Errorf("successor does not start from archived serving generation")
	}
	initial, e := t.j.Initial.canonical()
	if e != nil {
		return View{}, e
	}
	if initial.Phase != Prepare || initial.ExpiresAt.Sub(initial.IssuedAt) != p.RoundLifetime || initial.MaxReceiptAge != p.MaxReceiptAge {
		return View{}, fmt.Errorf("invalid initial phase round")
	}
	v := View{Round: initial}
	var validator *receiptValidator
	validate := func(at time.Time, proofs []AuthenticatedReceipt, complete bool) error {
		if validator == nil {
			prepared, err := v.Round.receiptValidator()
			if err != nil {
				return err
			}
			validator = &prepared
		}
		return validator.validate(at, proofs, complete)
	}
	last := initial.IssuedAt
	nonces := map[string]bool{initial.Nonce: true}
	for _, ev := range t.j.Events {
		if v.Complete || ev.At.Before(last) || ev.At.Before(v.Round.IssuedAt) {
			return View{}, fmt.Errorf("invalid transition chronology")
		}
		expected := v.Round.Spec
		switch ev.Operation {
		case observe:
			if ev.Next != nil {
				return View{}, fmt.Errorf("observation cannot replace the committed round")
			}
			proofs := make([]AuthenticatedReceipt, len(ev.Receipts))
			for i, receipt := range ev.Receipts {
				proofs[i] = AuthenticatedReceipt{AuthenticatedNodeUID: receipt.NodeUID, Receipt: receipt}
			}
			if e = validate(ev.At, proofs, false); e != nil {
				return View{}, e
			}
			byMember := map[[2]string]Receipt{}
			for _, receipt := range v.Observed {
				byMember[[2]string{receipt.NodeUID, receipt.DeviceID}] = receipt
			}
			for _, receipt := range ev.Receipts {
				id := [2]string{receipt.NodeUID, receipt.DeviceID}
				if old, found := byMember[id]; found && receipt.ObservedAt.Before(old.ObservedAt) {
					return View{}, fmt.Errorf("observation regresses member evidence")
				}
				byMember[id] = receipt
			}
			v.Observed = nil
			for _, member := range v.Round.Members {
				if receipt, found := byMember[[2]string{member.NodeUID, member.DeviceID}]; found {
					v.Observed = append(v.Observed, receipt)
				}
			}
			last = ev.At
			continue
		case advance:
			proofs := make([]AuthenticatedReceipt, len(ev.Receipts))
			for i, r := range ev.Receipts {
				proofs[i] = AuthenticatedReceipt{AuthenticatedNodeUID: r.NodeUID, Receipt: r}
			}
			if e = validate(ev.At, proofs, true); e != nil {
				return View{}, fmt.Errorf("invalid committed receipts: %w", e)
			}
			switch v.Round.Phase {
			case Prepare:
				expected.Phase = Switch
			case Switch:
				expected.Phase = Drain
				v.DrainUntil = ev.At.Add(p.DrainInterval)
			case Drain:
				if v.DrainUntil.IsZero() || ev.At.Before(v.DrainUntil) {
					return View{}, fmt.Errorf("drain interval has not elapsed")
				}
				for _, receipt := range ev.Receipts {
					if receipt.ObservedAt.Before(v.DrainUntil) {
						return View{}, fmt.Errorf("drain receipt predates drain deadline")
					}
				}
				expected.Phase = Retire
			case Retire:
				v.Complete = true
				v.ServingGeneration = v.Round.ToGeneration
			case Abort:
				v.Complete = true
				v.ServingGeneration = v.Round.FromGeneration
			default:
				return View{}, fmt.Errorf("invalid phase history")
			}
		case rollback:
			if len(ev.Receipts) != 0 || v.RollbackRequested {
				return View{}, fmt.Errorf("invalid repeated rollback")
			}
			switch v.Round.Phase {
			case Prepare:
				expected.Phase = Abort
			case Switch, Drain:
				expected.FromGeneration, expected.ToGeneration = expected.ToGeneration, expected.FromGeneration
				expected.Members = append([]Member(nil), expected.Members...)
				for i := range expected.Members {
					expected.Members[i].FromKey, expected.Members[i].ToKey = expected.Members[i].ToKey, expected.Members[i].FromKey
					expected.Members[i].FromPeerKey, expected.Members[i].ToPeerKey = expected.Members[i].ToPeerKey, expected.Members[i].FromPeerKey
				}
				expected.Phase = Switch
			default:
				return View{}, fmt.Errorf("cannot roll back after retirement is authorized")
			}
			v.RollbackRequested = true
			v.DrainUntil = time.Time{}
		case renew:
			if len(ev.Receipts) != 0 || ev.At.Before(v.Round.ExpiresAt) {
				return View{}, fmt.Errorf("only expired rounds can be renewed")
			}
		default:
			return View{}, fmt.Errorf("unknown transition operation")
		}
		if v.Complete {
			if ev.Next != nil {
				return View{}, fmt.Errorf("terminal event has a next round")
			}
		} else {
			if ev.Next == nil {
				return View{}, fmt.Errorf("missing next phase round")
			}
			next, err := ev.Next.canonical()
			if err != nil {
				return View{}, err
			}
			if !reflect.DeepEqual(next.Spec, expected) || !next.IssuedAt.Equal(ev.At) || next.ExpiresAt.Sub(next.IssuedAt) != p.RoundLifetime || next.MaxReceiptAge != p.MaxReceiptAge || nonces[next.Nonce] {
				return View{}, fmt.Errorf("next round changes intent or reuses identity")
			}
			nonces[next.Nonce] = true
			v.Round = next
			validator = nil
		}
		v.Observed = nil
		last = ev.At
	}
	return v, nil
}

// Extends accepts the same committed history or any number of appended events.
// It rejects regression and forks, including a newly minted initial round for
// the same plan. Callers must separately authenticate storage and object identity.
func (t Transition) Extends(previous Transition) error {
	old, err := previous.Bytes()
	if err != nil {
		return err
	}
	if _, err := t.View(); err != nil {
		return err
	}
	if len(t.j.Events) < len(previous.j.Events) {
		return fmt.Errorf("transition regresses committed history")
	}
	prefix := t.j
	prefix.Events = prefix.Events[:len(previous.j.Events)]
	raw, err := (Transition{prefix}).Bytes()
	if err != nil {
		return err
	}
	if !bytes.Equal(old, raw) {
		return fmt.Errorf("transition rewrites committed history")
	}
	return nil
}

func (t Transition) append(ev event) (Transition, error) {
	j := t.j
	j.Events = append(append([]event(nil), j.Events...), ev)
	next := Transition{j}
	if _, e := next.View(); e != nil {
		return Transition{}, e
	}
	return next, nil
}

// Observe records authenticated partial progress without advancing or renewing.
// Publication still requires the controller's storage compare-and-swap.
func (t Transition) Observe(now time.Time, receipts []AuthenticatedReceipt) (Transition, error) {
	v, err := t.View()
	if err != nil {
		return Transition{}, err
	}
	if v.Complete {
		return Transition{}, fmt.Errorf("transition is complete")
	}
	if err := v.Round.ValidateReceipts(now, receipts); err != nil {
		return Transition{}, err
	}
	ev := event{Operation: observe, At: now.UTC(), Receipts: make([]Receipt, len(receipts))}
	for i, proof := range receipts {
		ev.Receipts[i] = proof.Receipt
		ev.Receipts[i].ObservedAt = ev.Receipts[i].ObservedAt.UTC()
	}
	return t.append(ev)
}

func (t Transition) Advance(now time.Time, receipts []AuthenticatedReceipt) (Transition, error) {
	v, e := t.View()
	if e != nil {
		return Transition{}, e
	}
	if v.Complete {
		return Transition{}, fmt.Errorf("transition is complete")
	}
	if e = v.Round.Ready(now, receipts); e != nil {
		return Transition{}, e
	}
	ev := event{Operation: advance, At: now.UTC(), Receipts: make([]Receipt, len(receipts))}
	for i, proof := range receipts {
		ev.Receipts[i] = proof.Receipt
		ev.Receipts[i].ObservedAt = ev.Receipts[i].ObservedAt.UTC()
	}
	spec := v.Round.Spec
	switch spec.Phase {
	case Prepare:
		spec.Phase = Switch
	case Switch:
		spec.Phase = Drain
	case Drain:
		if now.Before(v.DrainUntil) {
			return Transition{}, fmt.Errorf("drain interval has not elapsed")
		}
		spec.Phase = Retire
	case Retire, Abort:
		return t.append(ev)
	}
	next, e := NewRound(spec, now, t.j.Policy.RoundLifetime, t.j.Policy.MaxReceiptAge)
	if e != nil {
		return Transition{}, e
	}
	ev.Next = &next
	return t.append(ev)
}

// Rollback during prepare requests removal of B while preserving A. Once any
// switch may have started, rollback first selects A everywhere, drains B, then
// retires B. Retirement already authorized must finish before a new transition.
func (t Transition) Rollback(now time.Time) (Transition, error) {
	v, e := t.View()
	if e != nil {
		return Transition{}, e
	}
	if v.Complete || v.RollbackRequested {
		return Transition{}, fmt.Errorf("rollback is unavailable")
	}
	spec := v.Round.Spec
	switch spec.Phase {
	case Prepare:
		spec.Phase = Abort
	case Switch, Drain:
		spec.FromGeneration, spec.ToGeneration = spec.ToGeneration, spec.FromGeneration
		spec.Members = append([]Member(nil), spec.Members...)
		for i := range spec.Members {
			spec.Members[i].FromKey, spec.Members[i].ToKey = spec.Members[i].ToKey, spec.Members[i].FromKey
			spec.Members[i].FromPeerKey, spec.Members[i].ToPeerKey = spec.Members[i].ToPeerKey, spec.Members[i].FromPeerKey
		}
		spec.Phase = Switch
	default:
		return Transition{}, fmt.Errorf("cannot roll back after retirement is authorized")
	}
	next, e := NewRound(spec, now, t.j.Policy.RoundLifetime, t.j.Policy.MaxReceiptAge)
	if e != nil {
		return Transition{}, e
	}
	return t.append(event{Operation: rollback, At: now.UTC(), Next: &next})
}

// Renew is explicit expiry recovery, not an observation-timeout retry. Hosts
// must observe the current phase again without reprovisioning its devices.
func (t Transition) Renew(now time.Time) (Transition, error) {
	v, e := t.View()
	if e != nil {
		return Transition{}, e
	}
	if v.Complete || now.Before(v.Round.ExpiresAt) {
		return Transition{}, fmt.Errorf("round is not expired or transition is complete")
	}
	next, e := NewRound(v.Round.Spec, now, t.j.Policy.RoundLifetime, t.j.Policy.MaxReceiptAge)
	if e != nil {
		return Transition{}, e
	}
	return t.append(event{Operation: renew, At: now.UTC(), Next: &next})
}
