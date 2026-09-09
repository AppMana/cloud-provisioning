package attachment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

type Phase string

const (
	Preparing   Phase = "Preparing"
	Publishing  Phase = "Publishing"
	Ready       Phase = "Ready"
	Withdrawing Phase = "Withdrawing"
	Releasing   Phase = "Releasing"
	Complete    Phase = "Complete"
)

// Record is durable intent. A backend uses LeaseID as its lease key;
// retried calls must reconcile that lease, not create a new set of resources.
type Record struct {
	ID     string      `json:"id"`
	Lease  string      `json:"lease"`
	Digest string      `json:"digest"`
	Plan   GatewayPlan `json:"plan"`
	Phase  Phase       `json:"phase"`
	// Version is the persistence backend's compare-and-swap token, not a
	// Kubernetes generation or a substitute for consumer acknowledgement.
	Version  string `json:"-"`
	Deleting bool   `json:"-"`
}

// LeaseID changes after completed retirement, even when identical desired
// configuration is attached again. A delayed release of the old lease must
// never remove the new attachment's ownership.
func (r Record) LeaseID() string { return r.ID + "/" + r.Lease + "/" + r.Digest }

// Store persists an attachment independently of any running reconciliation.
// Save must atomically compare previous.Version (nil means absent). Failure
// must not alter previous or next. The caller serializes Step for each ID;
// implementations still use CAS to reject competing controller writers.
type Store interface {
	Load(context.Context, string) (*Record, error)
	Save(context.Context, *Record, *Record) error
}

// Forwarder is a provider capability. Ensure revalidates the Machine, Node,
// network and interface identities and observes actual forwarding readiness.
// Release removes only resources still owned by this lease. Shared routes,
// ingress rules and gateway settings survive while another lease needs them.
// Both methods must persist their own resource intents before cloud mutations
// and be idempotent across a process crash or a failed Store.Save.
type Forwarder interface {
	Ensure(context.Context, Record) (bool, error)
	Release(context.Context, Record) (bool, error)
}

// Publication owns CNI address selection and the peer-list projection.
// Publish returns true only after the exact configuration has been applied by
// all required consumers. Withdraw returns true only after consumers confirm
// a configuration excluding this lease; the absence of a Pod is not an ack.
type Publication interface {
	Publish(context.Context, Record) (bool, error)
	Withdraw(context.Context, Record) (bool, error)
}

type Reconciler struct {
	Store       Store
	Forwarder   Forwarder
	Publication Publication
	Lifetime    Lifetime
}

// Lifetime protects participant infrastructure before preparation. Protect
// requests durable withdrawal when a participant starts deleting. Release runs
// only after publication withdrawal and all owned resource release completed.
type Lifetime interface {
	Protect(context.Context, Record) (retiring bool, err error)
	Release(context.Context, Record) error
}

func planDigest(plan GatewayPlan) string {
	raw, _ := json.Marshal(plan)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

// Step advances one persisted boundary. A changed desired attachment retires
// the old lease before preparing its replacement; old plan identities remain
// available through withdrawal and release. This is conservative replacement,
// not a claim of uninterrupted gateway failover.
func (r Reconciler) Step(ctx context.Context, id string, desired *GatewayRequest) (Phase, error) {
	if id == "" || r.Store == nil || r.Forwarder == nil || r.Publication == nil {
		return "", fmt.Errorf("attachment identity and all reconciliation capabilities are required")
	}
	current, err := r.Store.Load(ctx, id)
	if err != nil {
		return "", err
	}
	if current != nil && current.Deleting {
		desired = nil
	}
	var nextPlan GatewayPlan
	var nextDigest string
	if desired != nil {
		var err error
		nextPlan, err = PlanGateway(*desired)
		if err != nil {
			return "", err
		}
		nextDigest = planDigest(nextPlan)
	}
	if current != nil && (current.ID != id || current.Lease == "" || current.Digest != planDigest(current.Plan)) {
		return current.Phase, fmt.Errorf("persisted attachment identity or digest mismatch")
	}
	if current == nil || current.Phase == Complete {
		if current != nil && r.Lifetime != nil {
			if err := r.Lifetime.Release(ctx, *current); err != nil {
				return Complete, err
			}
		}
		if desired == nil {
			return Complete, nil
		}
		next := &Record{ID: id, Lease: rand.Text(), Digest: nextDigest, Plan: nextPlan, Phase: Preparing}
		if err := r.Store.Save(ctx, current, next); err != nil {
			return "", err
		}
		return Preparing, nil
	}
	if r.Lifetime != nil && desired != nil && current.Digest == nextDigest && (current.Phase == Preparing || current.Phase == Publishing || current.Phase == Ready) {
		retiring, err := r.Lifetime.Protect(ctx, *current)
		if err != nil {
			if current.Phase == Ready {
				phase, saveErr := r.transition(ctx, current, Publishing)
				return phase, errors.Join(err, saveErr)
			}
			return current.Phase, err
		}
		if retiring {
			desired = nil
		}
	}
	// Persist withdrawal before invoking any external removal. A controller
	// restart cannot forget why the old gateway must remain available.
	if (desired == nil || current.Digest != nextDigest) && (current.Phase == Preparing || current.Phase == Publishing || current.Phase == Ready) {
		return r.transition(ctx, current, Withdrawing)
	}
	var done bool
	var target Phase
	switch current.Phase {
	case Preparing:
		done, err = r.Forwarder.Ensure(ctx, *current)
		target = Publishing
	case Publishing, Ready:
		// Re-observe forwarding even after Ready so provider drift is surfaced.
		done, err = r.Forwarder.Ensure(ctx, *current)
		if err == nil && done {
			done, err = r.Publication.Publish(ctx, *current)
		}
		if err == nil && !done && current.Phase == Ready {
			return r.transition(ctx, current, Publishing)
		}
		target = Ready
	case Withdrawing:
		done, err = r.Publication.Withdraw(ctx, *current)
		target = Releasing
	case Releasing:
		done, err = r.Forwarder.Release(ctx, *current)
		target = Complete
	default:
		return current.Phase, fmt.Errorf("unknown attachment phase %q", current.Phase)
	}
	if err != nil {
		if current.Phase == Ready {
			phase, saveErr := r.transition(ctx, current, Publishing)
			return phase, errors.Join(err, saveErr)
		}
		return current.Phase, err
	}
	if !done || current.Phase == target {
		return current.Phase, nil
	}
	if target == Complete && r.Lifetime != nil {
		// Commit completion before releasing infrastructure holds. Retrying a
		// failed hook removal must not require a Machine that CAPI may now delete.
		phase, err := r.transition(ctx, current, Complete)
		if err != nil {
			return phase, err
		}
		if err := r.Lifetime.Release(ctx, *current); err != nil {
			return Complete, err
		}
		return Complete, nil
	}
	return r.transition(ctx, current, target)
}

func (r Reconciler) transition(ctx context.Context, current *Record, phase Phase) (Phase, error) {
	next := *current
	next.Phase = phase
	if err := r.Store.Save(ctx, current, &next); err != nil {
		return current.Phase, err
	}
	return phase, nil
}
