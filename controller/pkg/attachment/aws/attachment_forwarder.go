package aws

import (
	"context"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
)

type BindingResolver interface {
	Resolve(context.Context, attachment.Record) (ForwardingBinding, error)
}

// GatewayGuest reconciles and observes gateway IP forwarding and guest routes.
// Its cleanup must tolerate a deleted gateway and preserve other worker leases.
type GatewayGuest interface {
	Ensure(context.Context, attachment.Record, ForwardingBinding) (bool, error)
	Release(context.Context, attachment.Record, ForwardingBinding) (bool, error)
}

// AttachmentForwarder joins the provider-independent lifecycle to AWS. A guest
// implementation is mandatory: EC2 readiness cannot imply packet forwarding.
type AttachmentForwarder struct {
	Resolver BindingResolver
	Bound    BoundResources
	Guest    GatewayGuest
}

var _ attachment.Forwarder = AttachmentForwarder{}

func (f AttachmentForwarder) Ensure(ctx context.Context, record attachment.Record) (bool, error) {
	if f.Resolver == nil || f.Guest == nil {
		return false, fmt.Errorf("identity resolver and gateway guest capability required")
	}
	binding, err := f.Resolver.Resolve(ctx, record)
	if err != nil {
		return false, err
	}
	if binding.Lease != record.LeaseID() {
		return false, fmt.Errorf("resolved lease mismatch")
	}
	if ok, err := f.Bound.Ensure(ctx, binding); err != nil || !ok {
		return false, err
	}
	return f.Guest.Ensure(ctx, record, binding)
}
func (f AttachmentForwarder) Release(ctx context.Context, record attachment.Record) (bool, error) {
	if f.Bound.Store == nil || f.Guest == nil {
		return false, fmt.Errorf("binding store and gateway guest capability required")
	}
	saved, err := f.Bound.Store.Load(ctx, record.LeaseID())
	if err != nil {
		return false, err
	}
	if saved == nil || saved.Released {
		return true, nil
	}
	if saved.Binding.Lease != record.LeaseID() {
		return false, fmt.Errorf("persisted lease mismatch")
	}
	if ok, err := f.Guest.Release(ctx, record, saved.Binding); err != nil || !ok {
		return false, err
	}
	return f.Bound.Release(ctx, record.LeaseID())
}
