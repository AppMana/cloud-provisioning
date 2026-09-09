package attachment

import (
	"context"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// PublicationIntent retains the exact recipients and participants through
// withdrawal. A deleted or replaced consumer must not silently disappear.
type PublicationIntent struct {
	Lease      string                   `json:"lease"`
	Projection tunnel.GatewayProjection `json:"projection"`
	Consumers  []ConsumerTarget         `json:"consumers"`
	Retired    bool                     `json:"retired"`
	Version    string                   `json:"-"`
	Deleting   bool                     `json:"-"`
}
type PublicationStore interface {
	Load(context.Context, string) (*PublicationIntent, error)
	Save(context.Context, *PublicationIntent, *PublicationIntent) error
}
type PublicationResolver interface {
	Resolve(context.Context, Record) (*PublicationIntent, error)
}

// CNITransport owns its own durable address-change journal. Ensure selects and
// observes native transport; Restore uses recorded original values and must
// tolerate a deleted worker without changing a replacement Node.
type CNITransport interface {
	Ensure(context.Context, Record) (bool, error)
	Restore(context.Context, Record) (bool, error)
}
type GatewayPublication struct {
	Store           PublicationStore
	Resolver        PublicationResolver
	Projections     ProjectionStore
	Verifier        ConsumerVerifier
	Transport       CNITransport
	APIVIP, APIPort string
}

var _ Publication = GatewayPublication{}

func (p GatewayPublication) Publish(ctx context.Context, r Record) (bool, error) {
	if p.Store == nil || p.Resolver == nil || p.Transport == nil {
		return false, fmt.Errorf("publication store, resolver and CNI transport required")
	}
	intent, err := p.Store.Load(ctx, r.LeaseID())
	if err != nil {
		return false, err
	}
	if intent == nil {
		intent, err = p.Resolver.Resolve(ctx, r)
		if err != nil {
			return false, err
		}
		if intent == nil || intent.Lease != r.LeaseID() || intent.Projection.Lease != intent.Lease || len(intent.Consumers) == 0 || intent.Retired {
			return false, fmt.Errorf("incomplete publication intent")
		}
		if err = p.Store.Save(ctx, nil, intent); err != nil {
			return false, err
		}
		intent, err = p.Store.Load(ctx, r.LeaseID())
		if err != nil {
			return false, err
		}
	}
	if intent == nil || intent.Deleting || intent.Retired || intent.Lease != r.LeaseID() {
		return false, fmt.Errorf("publication intent missing or retiring")
	}
	mesh, err := p.Projections.Set(ctx, intent.Lease, &intent.Projection)
	if err != nil {
		return false, err
	}
	targets, intent, err := p.recipients(ctx, r, mesh, intent)
	if err != nil {
		return false, err
	}
	consumers, err := SnapshotConsumers(mesh, targets, p.APIVIP, p.APIPort)
	if err != nil {
		return false, err
	}
	if ok, err := p.Verifier.Applied(ctx, mesh, consumers); err != nil || !ok {
		return false, err
	}
	return p.Transport.Ensure(ctx, r)
}
func (p GatewayPublication) Withdraw(ctx context.Context, r Record) (bool, error) {
	if p.Store == nil || p.Transport == nil {
		return false, fmt.Errorf("publication store and CNI transport required")
	}
	intent, err := p.Store.Load(ctx, r.LeaseID())
	if err != nil {
		return false, err
	}
	if intent == nil || intent.Retired {
		return true, nil
	}
	if intent.Lease != r.LeaseID() {
		return false, fmt.Errorf("publication identity mismatch")
	}
	mesh, staged, err := p.Projections.RetireWorker(ctx, intent.Lease)
	if err != nil {
		return false, err
	}
	if staged {
		var targets []ConsumerTarget
		targets, intent, err = p.recipients(ctx, r, mesh, intent)
		if err != nil {
			return false, err
		}
		var worker []ConsumerTarget
		for _, target := range targets {
			if !target.Site && target.PublicKey == intent.Projection.WorkerKey {
				worker = append(worker, target)
			}
		}
		if len(worker) != 1 {
			return false, fmt.Errorf("withdrawal requires the exact retiring worker recipient")
		}
		consumers, err := SnapshotConsumers(mesh, worker, p.APIVIP, p.APIPort)
		if err != nil {
			return false, err
		}
		if ok, err := p.Verifier.Applied(ctx, mesh, consumers); err != nil || !ok {
			return false, err
		}
	}
	if ok, err := p.Transport.Restore(ctx, r); err != nil || !ok {
		return false, err
	}
	mesh, err = p.Projections.Set(ctx, intent.Lease, nil)
	if err != nil {
		return false, err
	}
	targets, intent, err := p.recipients(ctx, r, mesh, intent)
	if err != nil {
		return false, err
	}
	consumers, err := SnapshotConsumers(mesh, targets, p.APIVIP, p.APIPort)
	if err != nil {
		return false, err
	}
	if ok, err := p.Verifier.Applied(ctx, mesh, consumers); err != nil || !ok {
		return false, err
	}
	next := *intent
	next.Retired = true
	if err = p.Store.Save(ctx, intent, &next); err != nil {
		return false, err
	}
	return true, nil
}
